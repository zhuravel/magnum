package evalrun

import "github.com/zhuravel/magnum/internal/config"

// LocalCase is one round of the factory's local review: magnum's full
// review of a change in the task's copy, as a blind dry run, before any PR
// exists (docs/superpowers/specs/2026-11-03-factory-design.md, section 6.5).
// Checkout is the copy, BaseSHA and HeadSHA the change's ends, Branch its
// branch, Title (with the Jira key), Body and TicketMD what the judge reads
// from files instead of GitHub; Watch is the repository's [[watch]], whose
// review roles round 1 runs, Roles the roles of this round and Round its
// number, from 1. The factory's LocalReviewer takes it.
type LocalCase struct {
	Checkout, BaseSHA, HeadSHA, Branch, Title, Body, TicketMD string
	Watch                                                     config.Watch
	Roles                                                     []string
	Round                                                     int
}

// LocalResult is what a local round found: the round's Outcome (dry_run
// when it finished), its ReportDir, the judge's Verdict, the findings'
// counts by priority (P0 to P3) and, for a later round, the counts of the
// earlier findings still open, the findings themselves, and the round's
// number.
type LocalResult struct {
	Outcome, ReportDir, Verdict string
	Counts                      [4]int
	OpenCounts                  *[4]int
	Findings                    []LocalFinding
	Round                       int
}

// LocalFinding is one finding of a local round: its Severity (P0 to P3),
// Title, Path and Line, its Body, and whether it is a simplification or a
// question for the operator.
type LocalFinding struct {
	Severity, Title, Path    string
	Line                     int
	Body                     string
	Simplification, Question bool
}
