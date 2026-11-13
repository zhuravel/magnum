package engine

// The factory's lease of a PR's fixes, as magnum reads it (Deps.FixLeaseOf:
// the factory's registry, read only, across processes).

import "time"

// FixLease is what the factory's registry says of its fix work on a PR:
// Fixing, the factory holds the PR's lease and works on it now, in Phase
// (FixPhaseFixing while its fixer works, FixPhaseReviewing while it waits
// for magnum's re-review of its push), on Head, in cycle Cycle of Cap (0:
// not known); Worked, the factory worked the PR since its import (a start,
// a push, a reply or a change of its ledger), which keeps it the factory's
// (`magnum fix --handover back` refuses it); ReadAt, when the reader read
// it. The zero lease is a PR the factory has no record of.
type FixLease struct {
	Fixing bool
	Phase  string
	Head   string
	Cycle  int
	Cap    int
	Worked bool
	ReadAt time.Time
}
