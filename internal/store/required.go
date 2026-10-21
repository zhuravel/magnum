package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// RequiredChecks.Source values.
const (
	RequiredFromConfig = "config" // the [[repo]] block's required_checks
	RequiredFromGitHub = "github" // the default branch's rulesets or branch protection
)

// KVRepoRequiredChecks holds a repository's GitHubRequiredChecks (JSON),
// written by the poller.
func KVRepoRequiredChecks(fullName string) string {
	return "repo." + strings.ToLower(fullName) + ".required_checks"
}

// GitHubRequiredChecks is what the poller last read of the status checks a
// repository's default branch requires.
type GitHubRequiredChecks struct {
	Branch string   `json:"branch"`
	Checks []string `json:"checks"` // contexts, i.e. check run names or status contexts
	// Known is false when GitHub would not tell (403 on a private repository
	// of a free plan, 404 without admin access to the branch protection).
	Known     bool      `json:"known"`
	FetchedAt time.Time `json:"fetched_at,omitzero"` // the last read GitHub answered
	CheckedAt time.Time `json:"checked_at"`          // the last attempt
	// Error is why the last attempt failed; the fields above are what the
	// reads before it found.
	Error string `json:"error,omitempty"`
}

// GitHubRequiredChecks returns the cached GitHub list of repository
// fullName ("owner/name") and whether there is one.
func (s *Store) GitHubRequiredChecks(ctx context.Context, fullName string) (GitHubRequiredChecks, bool, error) {
	v, ok, err := s.GetKV(ctx, KVRepoRequiredChecks(fullName))
	if err != nil || !ok {
		return GitHubRequiredChecks{}, false, err
	}
	var g GitHubRequiredChecks
	if err := json.Unmarshal([]byte(v), &g); err != nil {
		return GitHubRequiredChecks{}, false, fmt.Errorf("required checks of %s: %w", fullName, err)
	}
	return g, true, nil
}

// SetGitHubRequiredChecks caches g for repository fullName.
func (s *Store) SetGitHubRequiredChecks(ctx context.Context, fullName string, g GitHubRequiredChecks) error {
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return s.SetKV(ctx, KVRepoRequiredChecks(fullName), string(b))
}

// RequiredChecks is the list of checks a repository's PRs must pass to be
// ready, and where it comes from.
type RequiredChecks struct {
	// Checks are check-name globs (path.Match, case-sensitive); entries from
	// the configuration may also be "workflow:<glob>", a whole GitHub
	// Actions workflow.
	Checks    []string  `json:"checks"`
	Source    string    `json:"source"`              // RequiredFromConfig | RequiredFromGitHub | "" (unknown)
	FetchedAt time.Time `json:"fetched_at,omitzero"` // when GitHub's list was read; zero for the configuration's
}

// RequiredChecks returns the checks PRs of repository fullName must pass:
// configured (config.Config.RequiredChecks) when it is not empty, else
// GitHub's as the poller last read them (Source "" and no checks when
// GitHub would not tell or was never asked). GitHub requiring nothing is
// Source RequiredFromGitHub with no checks.
func (s *Store) RequiredChecks(ctx context.Context, fullName string, configured []string) (RequiredChecks, error) {
	if len(configured) > 0 {
		return RequiredChecks{Checks: configured, Source: RequiredFromConfig}, nil
	}
	g, ok, err := s.GitHubRequiredChecks(ctx, fullName)
	if err != nil || !ok || !g.Known {
		return RequiredChecks{}, err
	}
	checks := g.Checks
	if checks == nil {
		checks = []string{}
	}
	return RequiredChecks{Checks: checks, Source: RequiredFromGitHub, FetchedAt: g.FetchedAt}, nil
}

// RequiredWorkflowPrefix marks a required check naming a whole GitHub
// Actions workflow ("workflow:CI"), as config.RequiredWorkflowPrefix does
// for [[repo]] required_checks.
const RequiredWorkflowPrefix = "workflow:"

// RequiredNames reports whether entry, a required check (a check-name glob,
// or RequiredWorkflowPrefix and a glob over GitHub Actions workflows) names
// c: a name glob matches c's name whatever its workflow, a check run's or a
// commit status's; a workflow entry every check of each workflow whose file
// ("ci.yml") or declared name ("CI") it matches (never a status or another
// app's check, which have neither). A workflow file a PR adds may declare
// the name of the base's: its runs are one more workflow's, kept apart by
// their file (Key), and the automatic approval holds such a PR before its
// CI gate reads them. A malformed glob names nothing.
func RequiredNames(entry string, c CheckResult) bool {
	if glob, ok := strings.CutPrefix(entry, RequiredWorkflowPrefix); ok {
		if c.Status || c.Workflow == "" {
			return false
		}
		byName, _ := path.Match(glob, c.Workflow)
		byFile, _ := path.Match(glob, c.WorkflowFile)
		return byName || byFile && c.WorkflowFile != ""
	}
	m, _ := path.Match(entry, c.Name)
	return m
}

// RequiredRuns are the runs of checks that count for entry, a required
// check (RequiredNames), in checks' order: every check of a workflow
// entry's workflows; for a name glob the latest check run of each matching
// name across workflows (a check posted through the API lands in another
// workflow's suite, next to an older run) and, apart, the latest commit
// status of that name, which anyone who can write to the repository posts,
// so it never stands for a check run. None when entry names no check. The
// board's required checks and the auto-approval's CI gate read them.
func RequiredRuns(checks []CheckResult, entry string) []CheckResult {
	var out []CheckResult
	if strings.HasPrefix(entry, RequiredWorkflowPrefix) {
		for _, c := range checks {
			if RequiredNames(entry, c) {
				out = append(out, c)
			}
		}
		return out
	}
	type key struct {
		status bool
		name   string
	}
	at := map[key]int{} // a check run's or a status's name -> its run's index in out
	for _, c := range checks {
		if !RequiredNames(entry, c) {
			continue
		}
		k := key{c.Status, c.Name}
		i, seen := at[k]
		switch {
		case !seen:
			at[k] = len(out)
			out = append(out, c)
		case newerRun(c, out[i]):
			out[i] = c
		}
	}
	return out
}

// newerRun reports whether run a is newer than b: a run not started yet
// (zero At) is the newest.
func newerRun(a, b CheckResult) bool {
	switch {
	case a.At.IsZero():
		return true
	case b.At.IsZero():
		return false
	}
	return a.At.After(b.At)
}

// CheckKey is the check a CheckResult is a run of (CheckResult.Key).
type CheckKey struct {
	Status   bool   // a commit status
	Workflow string // a check run's workflow file, else its workflow's name; "" for a status or another app's check
	Name     string // the check run's name or the status's context
}

// Key is the check c is a run of: a check run's workflow file (its
// workflow's name for a run read without one, by an earlier build) and
// name, or a commit status's context. Two workflow files that declare the
// same name are two workflows, and a status is never a run of a check run's
// check, so neither replaces the other's runs.
func (c CheckResult) Key() CheckKey {
	if c.Status {
		return CheckKey{Status: true, Name: c.Name}
	}
	return CheckKey{Workflow: cmp.Or(c.WorkflowFile, c.Workflow), Name: c.Name}
}
