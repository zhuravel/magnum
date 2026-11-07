package github

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// workflowFileRe is a workflow's file name in .github/workflows ("ci.yml"):
// no directory, no "..", a .yml or .yaml file.
var workflowFileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.ya?ml$`)

// ValidWorkflowFile reports whether name is a workflow file DispatchWorkflow
// takes: a plain file name of .github/workflows ending in .yml or .yaml.
func ValidWorkflowFile(name string) bool {
	return workflowFileRe.MatchString(name) && !strings.Contains(name, "..")
}

// DispatchWorkflow starts a run of the workflow file workflow ("ci.yml") on
// ref, a branch of owner/repo, as the client's identity: POST
// /repos/{o}/{r}/actions/workflows/{file}/dispatches. GitHub runs the
// workflow as the branch has it, at its tip, and refuses (HTTP 422) one whose
// file there has no workflow_dispatch trigger; the identity needs the right
// to run workflows (a gh login that can push, an App with Actions: write).
// The branch, which a PR names, goes as JSON on gh's stdin, so a failing call
// logs none of it. A workflow that is not a plain file name of
// .github/workflows (ValidWorkflowFile) or an empty ref is refused before
// any call. It is marked Mutates, so execx.DryRun only plans it.
func (c *Client) DispatchWorkflow(ctx context.Context, owner, repo, workflow, ref string) error {
	if err := checkRepo(owner, repo); err != nil {
		return err
	}
	if !ValidWorkflowFile(workflow) {
		return fmt.Errorf("github: invalid workflow file %q", workflow)
	}
	if strings.TrimSpace(ref) == "" || strings.ContainsRune(ref, 0) {
		return fmt.Errorf("github: dispatching %s needs a branch", workflow)
	}
	path := fmt.Sprintf("repos/%s/%s/actions/workflows/%s/dispatches", owner, repo, workflow)
	op := fmt.Sprintf("dispatch %s/%s:%s", owner, repo, workflow)
	return c.restInput(ctx, op, "POST", path, map[string]string{"ref": ref}, true, nil)
}
