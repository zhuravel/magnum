package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// requiredChecksTTL is how long the checks GitHub requires on a repository's
// default branch are reused; requiredChecksRetry is how soon a failed read
// (and a repository's first sync) asks again.
const (
	requiredChecksTTL   = 6 * time.Hour
	requiredChecksRetry = 30 * time.Minute
)

// refreshRequiredChecks re-reads the status checks GitHub requires on
// branch (the repository's default branch) into the registry when the cached
// list is older than requiredChecksTTL or for another branch, on the
// repository's first sync, and after a failure once requiredChecksRetry
// passed. A failed read keeps the cached list. The poller calls it for
// repositories with open PRs only; the board reads the list through
// store.RequiredChecks, where [[repo]] required_checks overrides it.
func (e *Engine) refreshRequiredChecks(ctx context.Context, gh GitHub, repo store.Repo, branch string, firstSync bool, now time.Time) {
	full := repo.FullName()
	cur, ok, err := e.st.GitHubRequiredChecks(ctx, full)
	if err != nil {
		e.log.Warn("read required checks", "repo", full, "err", err)
	}
	age := now.Sub(cur.CheckedAt)
	if ok && cur.Branch == branch && age < requiredChecksTTL && (age < requiredChecksRetry || cur.Error == "" && !firstSync) {
		return
	}
	checks, known, err := gh.RequiredChecks(ctx, repo.Owner, repo.Name, branch)
	next := store.GitHubRequiredChecks{Branch: branch, Checks: checks, Known: known, FetchedAt: now, CheckedAt: now}
	if err != nil {
		next = cur // the last list GitHub gave stands
		next.Branch, next.CheckedAt, next.Error = branch, now, err.Error()
		if e.changed("required:"+full, err.Error()) {
			e.event(ctx, "warn", "repo:"+full, "poll.required_checks_error", fmt.Sprintf("required checks of %s: %v", branch, err), nil)
		}
	} else {
		e.changed("required:"+full, "")
		if !ok || cur.Known != known || !slices.Equal(cur.Checks, checks) {
			msg := "GitHub does not say which checks " + branch + " requires"
			switch {
			case known && len(checks) == 0:
				msg = branch + " requires no checks"
			case known:
				msg = branch + " requires " + strings.Join(checks, ", ")
			}
			e.event(ctx, "info", "repo:"+full, "repo.required_checks", msg, nil)
		}
	}
	if err := e.st.SetGitHubRequiredChecks(ctx, full, next); err != nil {
		e.log.Warn("record required checks", "repo", full, "err", err)
	}
}
