package github

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// DispatchWorkflow starts a workflow_dispatch run of a workflow file on a
// branch (POST .../actions/workflows/<file>/dispatches) as the client's
// identity. The branch, a PR's own text, goes as JSON on gh's stdin, never
// in argv (a failing call logs argv); the call is marked Mutates, so a dry
// run only plans it.
func TestDispatchWorkflowPostsTheBranchOnStdin(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api", "-X", "POST"}}}}
	c := &Client{Run: f, Env: map[string]string{"GH_CONFIG_DIR": "/state/gh/zhuravel"}}
	if err := c.DispatchWorkflow(context.Background(), "talkable", "talkable", "ci.yml", "feature/fix-it"); err != nil {
		t.Fatal(err)
	}
	cmd := f.Calls[0]
	want := []string{"api", "-X", "POST", "repos/talkable/talkable/actions/workflows/ci.yml/dispatches", "--hostname", "github.com", "--input", "-"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %q", cmd.Args)
	}
	var sent map[string]any
	if err := json.Unmarshal(cmd.Stdin, &sent); err != nil || len(sent) != 1 || sent["ref"] != "feature/fix-it" {
		t.Errorf("stdin = %s, %v", cmd.Stdin, err)
	}
	if !cmd.Mutates || cmd.Env["GH_CONFIG_DIR"] != "/state/gh/zhuravel" {
		t.Errorf("mutates = %v, env = %v", cmd.Mutates, cmd.Env)
	}

	dry := &execx.DryRun{Inner: &execx.Fake{}}
	if err := (&Client{Run: dry}).DispatchWorkflow(context.Background(), "talkable", "talkable", "ci.yml", "main"); err != nil {
		t.Fatal(err)
	}
	if len(dry.Planned) != 1 {
		t.Errorf("planned = %d, want the dispatch planned and not run", len(dry.Planned))
	}
}

// A workflow GitHub will not start on the branch (its file there has no
// workflow_dispatch trigger) answers HTTP 422: an APIError with that status.
// A workflow that is not a plain file name of .github/workflows, or an empty
// branch, is refused before any call.
func TestDispatchWorkflowRefusals(t *testing.T) {
	f := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"gh", "api"}, Result: execx.Result{
		Stdout: compact(t, `{"message":"Workflow does not have 'workflow_dispatch' trigger","documentation_url":"https://docs.github.com/rest/actions/workflows#create-a-workflow-dispatch-event","status":"422"}`),
		Stderr: []byte("gh: Workflow does not have 'workflow_dispatch' trigger (HTTP 422)\n"), Code: 1}}}}
	err := (&Client{Run: f}).DispatchWorkflow(context.Background(), "talkable", "talkable", "ci.yml", "main")
	var api *APIError
	if !errors.As(err, &api) || api.Status != 422 {
		t.Fatalf("err = %v, want an APIError of HTTP 422", err)
	}
	for _, tc := range []struct{ workflow, ref string }{
		{"../ci.yml", "main"}, {"ci", "main"}, {"sub/ci.yml", "main"}, {"", "main"}, {"ci.yml", ""}, {"ci.yml", " "},
	} {
		f := &execx.Fake{}
		if err := (&Client{Run: f}).DispatchWorkflow(context.Background(), "talkable", "talkable", tc.workflow, tc.ref); err == nil ||
			len(f.Calls) != 0 || !strings.HasPrefix(err.Error(), "github: ") {
			t.Errorf("DispatchWorkflow(%q, %q) = %v after %d calls, want a refusal before any", tc.workflow, tc.ref, err, len(f.Calls))
		}
	}
}
