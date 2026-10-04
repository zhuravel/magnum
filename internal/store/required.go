package store

import (
	"context"
	"encoding/json"
	"fmt"
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
