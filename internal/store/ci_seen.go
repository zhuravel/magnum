package store

// The checks magnum sees (DECISIONS "ignore_checks makes magnum treat a
// named check as absent wherever it reads CI"): a third-party app's check
// that failed on every PR of talkable/talkable from 2026-10-26 kept GitHub's
// rollup at FAILURE, so clean reviews posted as comments, the CI-red
// follow-up queued judge checks for it and the automatic approval waited
// for it. [[watch]] and [[repo]] ignore_checks (config.Config.IgnoreChecks)
// name such checks, and every reader of a head's CI reads it through SeenCI
// or PR.IgnoringChecks: the judge's failing-checks.json, the CI-red
// follow-up, the approval gates and the board.

import (
	"slices"
	"strings"
)

// SeenCI is ci, the stored checks of a PR whose head is head, as magnum
// reads them where ignore (config.Config.IgnoreChecks: check-name globs and
// RequiredWorkflowPrefix entries, matched as RequiredNames matches) names
// some of them: those left out, the counts tallied again (Tally) and Total
// less them, and, when they are head's checks and every one (Complete),
// State derived from the rest: FAILURE when one of them failed, PENDING
// when one runs, SUCCESS when they passed or skipped, "" (no checks) when
// none is left; an ignored check that failed no longer holds GitHub's
// rollup at FAILURE. Checks of another commit, or a list GitHub cut at 100,
// keep GitHub's rollup State: magnum cannot tell what the rest did. ci
// itself when ignore names none of its checks (nil for nil); ci is never
// changed, and Rollup (GitHub's own counts, which the poll compares to
// tell a change) stays as read.
func SeenCI(ci *CIStatus, head string, ignore []string) *CIStatus {
	seen, _ := seenCI(ci, head, ignore)
	return seen
}

// seenCI is SeenCI and whether it derived State again.
func seenCI(ci *CIStatus, head string, ignore []string) (*CIStatus, bool) {
	if ci == nil || len(ignore) == 0 {
		return ci, false
	}
	ignored := func(c CheckResult) bool {
		return slices.ContainsFunc(ignore, func(entry string) bool { return RequiredNames(entry, c) })
	}
	if !slices.ContainsFunc(ci.Checks, ignored) {
		return ci, false
	}
	out := *ci
	out.Checks = slices.DeleteFunc(slices.Clone(ci.Checks), ignored)
	out.Total = max(ci.Total-(len(ci.Checks)-len(out.Checks)), len(out.Checks))
	out.Tally()
	if ci.SHA == "" || ci.SHA != head || !ci.Complete {
		return &out, false
	}
	switch {
	case out.Failed > 0:
		out.State = "FAILURE"
	case out.Pending > 0:
		out.State = "PENDING"
	case len(out.Checks) == 0:
		out.State = ""
	default:
		out.State = "SUCCESS"
	}
	return &out, true
}

// IgnoringChecks is p with its checks as magnum reads them where ignore
// names some (SeenCI on p's head) and, when SeenCI derived their state
// again and p's rollup (CIState, which the approval's CI gate reads) is the
// one the checks were read with (a rollup that moved since, after a Details
// fetch that failed, is newer than the checks: it stays), CIState that
// state too. A rollup the registry never read stays unread. p itself when
// ignore names none of its checks; p's CI and CIState are never changed.
func (p PR) IgnoringChecks(ignore []string) PR {
	ci, derived := seenCI(p.CI, p.HeadSHA, ignore)
	if ci == p.CI {
		return p
	}
	if derived && p.CIState != nil && strings.EqualFold(*p.CIState, p.CI.State) {
		p.CIState = new(ci.State)
	}
	p.CI = ci
	return p
}
