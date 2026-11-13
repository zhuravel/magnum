package engine

// The verdict autofix = "open" keeps per own PR (KVPROpen), and the seen
// ledger behind it (kvPRFixSeen): what the look (fix_open.go) decided at
// the PR's head, in plain words, and what it remembers of the items it saw,
// the loops it saw start and end and the keys their fixers had. The engine
// is their only writer; the factory's screen reads the verdict.
//
// decideOpen (fixwork.DecideOpen) is the decision, a pure function of the
// facts the look gathers (openFacts), by the rules fixwork/open.go lists.

import (
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

// KVPROpen holds autofix = "open"'s verdict on an own PR (OpenVerdict as
// JSON), written by the engine's look; no row for a PR the look does not
// cover.
func KVPROpen(prID int64) string { return fmt.Sprintf("pr.%d.open", prID) }

// kvPRFixSeen holds the look's ledger of the PR (openLedger as JSON).
func kvPRFixSeen(prID int64) string { return fmt.Sprintf("pr.%d.fix_seen", prID) }

// openCIOf is rule 11's CI of pr's head by required, its repository's
// required checks (store.Store.RequiredChecks: the [[repo]] block's, else
// GitHub's for the default branch), and enforced, the checks GitHub
// requires on the PR's base with no run at the head (enforcedMissing): a
// known empty list leaves CI out; the head's stored checks decide where
// they can (requiredCIOf, where strict, the [[repo]] list of a repository
// that sets ci_dispatch, makes an entry with no run missing, never green;
// a required run that failed, or one skipped beside a failed job of its
// workflow file, failedRequired, fails it), and GitHub's rollup otherwise,
// a required list with no rollup at all (no check has run) waiting for it.
// An enforced check is missing either way, and goes before the rollup's
// failure, which may be a check nobody requires.
func openCIOf(pr store.PR, required store.RequiredChecks, strict bool, enforced []string) openCI {
	if required.Source != "" && len(required.Checks) == 0 {
		return openCI{}
	}
	ci := openCI{Missing: enforced}
	if r, ok := requiredCIOf(pr, required.Checks, strict); ok {
		if failed := failedRequired(pr, required.Checks, r); len(failed) > 0 {
			return openCI{Failed: missingWhat(ciEntries(failed)), FailedRuns: failed, Pending: r.running}
		}
		ci.Pending, ci.Missing = r.running, withMissing(r.missing, enforced)
	} else {
		switch deref(pr.CIState) {
		case "FAILURE", "ERROR":
			if len(enforced) == 0 {
				return openCI{Failed: "CI"}
			}
		case "PENDING", "EXPECTED":
			ci.Pending = true
		case "":
			ci.Pending = len(required.Checks) > 0
		}
	}
	if len(ci.Missing) > 0 && pr.CI != nil {
		ci.Running = slices.ContainsFunc(pr.CI.Checks, func(c store.CheckResult) bool { return c.State == store.CheckPending })
	}
	return ci
}

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
