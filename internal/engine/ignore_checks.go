package engine

// The checks the engine's CI readers see (DECISIONS "ignore_checks makes
// magnum treat a named check as absent wherever it reads CI"): a check a
// repository's ignore_checks names (config.Config.IgnoreChecks: its
// watch's list and its [[repo]] block's) is left out of the head's stored
// checks, and GitHub's rollup, which such a check that failed holds at
// FAILURE, is derived again from the rest (store.PR.IgnoringChecks), before
// the approval's CI gate (ciGateOf: the automatic approval, its standing
// re-gate and the judge's approval gate), its ignored-checks note and the
// CI-red follow-up (ciRedOf, ciPollWorth) read them. The poll's change
// detection still compares GitHub's own rollup and counts, which can read
// the checks again. The globs are the operator's configuration, never a
// PR's, and no event or log names a check for them.

import (
	"context"
	"slices"

	"github.com/zhuravel/magnum/internal/store"
)

// seenPR is pr, a PR of repository full ("owner/name"), with its checks as
// magnum reads them: those full's ignore_checks names left out
// (store.PR.IgnoringChecks); pr itself when it names none of them.
func (e *Engine) seenPR(full string, pr store.PR) store.PR {
	return pr.IgnoringChecks(e.cfg.IgnoreChecks(full))
}

// seenFailures is pr with its checks as magnum reads them (seenPR) for a
// reader that counts only failed checks (the CI-red follow-up), which does
// not hold pr's repository: it is read from the registry only when one of
// pr's stored checks failed, since ignore_checks changes nothing else for
// it. ok is false when that read fails.
func (e *Engine) seenFailures(ctx context.Context, pr store.PR) (store.PR, bool) {
	if pr.CI == nil || !slices.ContainsFunc(pr.CI.Checks, failedCheck) {
		return pr, true
	}
	repo, err := e.st.RepoByID(ctx, pr.RepoID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "the PR's repository for its ignore_checks", "pr", pr.ID)
		return pr, false
	}
	return e.seenPR(repo.FullName(), pr), true
}

// failedCheck reports whether c failed.
func failedCheck(c store.CheckResult) bool { return c.State == store.CheckFailed }
