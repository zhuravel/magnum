package factory

// The overlap line of a task (spec 6.1): what else touches its work. The
// commits that name its key on any branch but its own (`git log --all
// --grep=<key>` in the repository's main clone), the PRs that name the key
// (`gh pr list --search <key>`, the task's own left out) and the open PRs
// that change the files its report names (read again once the report names
// them). The line is advisory: it never stops a task.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/textx"
)

// The bounds of the overlap's reads.
const (
	overlapTimeout = 30 * time.Second
	// overlapCommits is the most commits git log lists, overlapPRs the most
	// PRs that name the key, overlapOpen the most open PRs whose files are
	// read.
	overlapCommits = 50
	overlapPRs     = 20
	overlapOpen    = 100
)

// Overlap is the advisory line of what else touches task t's work ("" for
// nothing): the commits that name its key, the PRs that name it, and the
// open PRs that change files (the report's). Each part is read on its own;
// one that fails leaves the others' line, and the error says what failed.
// A build without a runner (Deps.Run) reads nothing.
func (e *Engine) Overlap(ctx context.Context, t registry.Task, files []string) (string, error) {
	if e.d.Run == nil {
		return "", nil
	}
	repo, ok := e.cfg.FactoryRepo(t.Repo)
	if !ok {
		return "", fmt.Errorf("overlap of %s: no [[factory.repo]] for %q", TaskKey(t), t.Repo)
	}
	var parts []string
	var errs []error
	add := func(part string, err error) {
		if part != "" {
			parts = append(parts, part)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if t.JiraKey != "" {
		add(e.keyCommits(ctx, t, repo.MainClone))
		add(e.keyPRs(ctx, t))
	}
	if len(files) > 0 {
		add(e.filePRs(ctx, t, files))
	}
	if len(parts) == 0 {
		return "", errors.Join(errs...)
	}
	return "overlap: " + strings.Join(parts, " · "), errors.Join(errs...)
}

// keyCommits names the commits of the main clone, on every ref but the
// task's own branch (local and origin's), whose message names its key.
func (e *Engine) keyCommits(ctx context.Context, t registry.Task, clone string) (string, error) {
	if clone == "" {
		return "", nil
	}
	args := []string{"log"}
	if t.Branch != "" {
		args = append(args, "--exclude=refs/heads/"+t.Branch, "--exclude=refs/remotes/origin/"+t.Branch)
	}
	args = append(args, "--all", "--fixed-strings", "--grep="+t.JiraKey, "--format=%h%x09%s", "-n", strconv.Itoa(overlapCommits))
	res, err := e.d.Run.Run(ctx, execx.Cmd{Name: "git", Args: args, Dir: clone, Timeout: overlapTimeout,
		Label: "git log --grep " + t.JiraKey + " (overlap)"})
	if err != nil {
		return "", fmt.Errorf("the commits that name %s: %w", t.JiraKey, err)
	}
	var shas []string
	for line := range strings.Lines(res.Out()) {
		if sha, _, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && sha != "" {
			shas = append(shas, sha)
		}
	}
	if len(shas) == 0 {
		return "", nil
	}
	verb := "name"
	if len(shas) == 1 {
		verb = "names"
	}
	return fmt.Sprintf("%s %s %s (newest %s)", textx.Count(len(shas), "commit", "commits"), verb, t.JiraKey, shas[0]), nil
}

// ghPR is a PR as gh pr list --json prints it.
type ghPR struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Files  []struct {
		Path string `json:"path"`
	} `json:"files"`
}

// ghList runs gh pr list on the task's repository with args and decodes
// its JSON: a read of github.com as your gh login.
func (e *Engine) ghList(ctx context.Context, t registry.Task, what string, args ...string) ([]ghPR, error) {
	argv := append([]string{"pr", "list", "--repo", t.Repo}, args...)
	res, err := e.d.Run.Run(ctx, execx.Cmd{Name: "gh", Args: argv, Env: map[string]string{"GH_HOST": "github.com"},
		Timeout: overlapTimeout, Label: "gh pr list (overlap: " + what + ")"})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	var prs []ghPR
	if err := json.Unmarshal(res.Stdout, &prs); err != nil {
		return nil, fmt.Errorf("%s: gh printed no PR list: %w", what, err)
	}
	return slices.DeleteFunc(prs, func(p ghPR) bool { return p.Number == t.PRNumber && t.PRNumber > 0 }), nil
}

// keyPRs names the PRs that name the task's key, in any state.
func (e *Engine) keyPRs(ctx context.Context, t registry.Task) (string, error) {
	prs, err := e.ghList(ctx, t, "the PRs that name "+t.JiraKey,
		"--search", strconv.Quote(t.JiraKey), "--state", "all", "--json", "number,state,title", "--limit", strconv.Itoa(overlapPRs))
	if err != nil || len(prs) == 0 {
		return "", err
	}
	names := make([]string, len(prs))
	for i, p := range prs {
		names[i] = fmt.Sprintf("#%d (%s)", p.Number, strings.ToLower(p.State))
	}
	if len(prs) == 1 {
		return fmt.Sprintf("PR %s names %s", names[0], t.JiraKey), nil
	}
	return fmt.Sprintf("PRs %s name %s", strings.Join(names, ", "), t.JiraKey), nil
}

// filePRs names the open PRs that change one of files: the file, or how
// many of them.
func (e *Engine) filePRs(ctx context.Context, t registry.Task, files []string) (string, error) {
	prs, err := e.ghList(ctx, t, "the open PRs' files",
		"--state", "open", "--json", "number,files", "--limit", strconv.Itoa(overlapOpen))
	if err != nil {
		return "", err
	}
	var out []string
	for _, p := range prs {
		var shared []string
		for _, f := range p.Files {
			if slices.Contains(files, f.Path) && !slices.Contains(shared, f.Path) {
				shared = append(shared, f.Path)
			}
		}
		switch len(shared) {
		case 0:
		case 1:
			out = append(out, fmt.Sprintf("#%d changes %s", p.Number, shared[0]))
		default:
			out = append(out, fmt.Sprintf("#%d changes %d of its files", p.Number, len(shared)))
		}
	}
	return strings.Join(out, ", "), nil
}
