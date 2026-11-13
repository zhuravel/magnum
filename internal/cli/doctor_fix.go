package cli

// The doctor's line of the fix loop's move into the factory (DECISIONS "The
// fix loop moves into the factory"): the PRs whose fix work of magnum's
// keeps them from handing over (the drain proof's morning list,
// engine.FixInFlight), and the PRs whose factory lease, or whose owner row
// in magnum's registry, could not be read, which wait for the grace or
// were reviewed without it (failed open, engine.FixLeaseUnreads).

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/textx"
)

// doctorFixHandover is the fix handover's lines: a WARN while a PR that
// hands over to the factory waits for its fix work to end, or while a read
// of the factory's lease or of an owner row failed (doctorLeaseUnreads),
// and a PASS that counts magnum's fix work in flight otherwise.
func doctorFixHandover(ctx context.Context, d doctorDeps) []doctorCheck {
	if d.Store == nil || d.Config == nil {
		return nil
	}
	const name = "fix handover"
	list, err := engine.FixInFlight(ctx, d.Store, d.Config, time.Now())
	if err != nil {
		return []doctorCheck{doctorWarned(name, "magnum's fix work in flight cannot be read: "+err.Error(), "")}
	}
	unread, err := engine.FixLeaseUnreads(ctx, d.Store)
	if err != nil {
		return []doctorCheck{doctorWarned(name, "the failed reads of the factory's leases cannot be read: "+err.Error(), "")}
	}
	var out []doctorCheck
	var waiting []string
	for _, u := range list {
		if u.Handing {
			waiting = append(waiting, fmt.Sprintf("%s (%s)", actRefLabel(d.Config.Daemon.DefaultRepo, u.Repo, u.Number), recordKinds(u.Records)))
		}
	}
	// A held fix holds no PR back: it goes with its PR, for the factory's
	// import (engine/fix_drained.go).
	held := ""
	if keys, err := d.Store.KVKeys(ctx, "pr.*.fix_held"); err == nil && len(keys) > 0 {
		held = fmt.Sprintf("; %s no PR back, each going with its PR for the factory to import",
			textx.Count(len(keys), "held fix holds", "held fixes hold"))
	}
	switch {
	case len(waiting) > 0:
		out = append(out, doctorWarned(name, fmt.Sprintf("%d PRs wait to hand over to the factory until magnum's fix work on them ends: %s%s",
			len(waiting), strings.Join(waiting, ", "), held), "`magnum fix --drained` names your action for each"))
	case len(list) > 0:
		out = append(out, doctorOK(name, fmt.Sprintf("magnum's fix work is in flight on %d PRs (`magnum fix --drained` lists it)%s", len(list), held)))
	default:
		out = append(out, doctorOK(name, "none of magnum's fix work is in flight: every PR can hand over to the factory"+held))
	}
	return append(out, doctorLeaseUnreads(ctx, d, unread)...)
}

// doctorLeaseUnreads are the lines of the PRs whose failed reads hold or
// held their rounds (engine.LeaseUnread), one per source: a WARN named for
// the factory's lease, and one named for magnum's own owner rows, each
// naming the PRs magnum reviews without the read (failed open) and those
// whose round waits for the grace; none for no such PR.
func doctorLeaseUnreads(ctx context.Context, d doctorDeps, unread map[int64]engine.LeaseUnread) []doctorCheck {
	var leases, owners leaseGroup
	parsed, registry := false, false
	for _, id := range slices.Sorted(maps.Keys(unread)) {
		u := unread[id]
		label := fmt.Sprintf("PR %d", id)
		if pr, err := d.Store.PRByID(ctx, id); err == nil {
			if repo, err := d.Store.RepoByID(ctx, pr.RepoID); err == nil {
				label = actRefLabel(d.Config.Daemon.DefaultRepo, repo.FullName(), pr.Number)
			}
		}
		why, g := cmp.Or(actClean(u.Err), "no error"), &leases
		switch u.Source { // magnum's own row, not the factory's registry
		case engine.LeaseFromOwner:
			why, g, parsed = "its owner row in magnum's registry: "+why, &owners, true
		case engine.LeaseFromRegistry:
			why, g, registry = "magnum's registry cannot read it: "+why, &owners, true
		}
		g.add(fmt.Sprintf("%s since %s (%s)", label, u.Since.Local().Format("Jan 2 15:04"), why), u.Open)
	}
	var out []doctorCheck
	if c, ok := leases.check("factory lease", "the factory fixes without its lease, which it cannot read", "the factory's lease, which cannot be read",
		"the factory's registry cannot be read; `magnum factory status` says whether its daemon runs"); ok {
		out = append(out, c)
	}
	var fixes []string
	if registry {
		fixes = append(fixes, "magnum's registry cannot read an owner row (pr.*.fix_owner): its database is busy or damaged; "+
			"the registry line says whether it reads")
	}
	if parsed {
		fixes = append(fixes, "magnum's registry holds an owner row (pr.*.fix_owner) this build cannot parse: a newer build wrote it, "+
			"or it is damaged; run the build that wrote it")
	}
	if c, ok := owners.check("fix owner row", "without their owner row, which it cannot read", "their owner row, which cannot be read",
		strings.Join(fixes, "; ")); ok {
		out = append(out, c)
	}
	return out
}

// leaseGroup is the PRs of one source of failed reads: those magnum reviews
// without the read (open) and those whose round waits for the grace.
type leaseGroup struct{ open, waits []string }

func (g *leaseGroup) add(what string, open bool) {
	if open {
		g.open = append(g.open, what)
	} else {
		g.waits = append(g.waits, what)
	}
}

// check is g's WARN, name, its headlines naming what magnum reviews the PRs
// without (reviews) and what their rounds wait for (waits), and fix; ok
// false for no PR.
func (g leaseGroup) check(name, reviews, waits, fix string) (doctorCheck, bool) {
	var parts []string
	if len(g.open) > 0 {
		parts = append(parts, fmt.Sprintf("magnum reviews %d PRs %s (failed open): %s", len(g.open), reviews, strings.Join(g.open, ", ")))
	}
	if len(g.waits) > 0 {
		parts = append(parts, fmt.Sprintf("%d PRs' rounds wait for %s: %s", len(g.waits), waits, strings.Join(g.waits, ", ")))
	}
	if len(parts) == 0 {
		return doctorCheck{}, false
	}
	return doctorWarned(name, strings.Join(parts, "; "), fix), true
}

// recordKinds are recs' kinds, in a few words.
func recordKinds(recs []engine.FixRecord) string {
	kinds := make([]string, 0, len(recs))
	for _, r := range recs {
		kinds = append(kinds, strings.ReplaceAll(r.Kind, "_", " "))
	}
	return strings.Join(kinds, ", ")
}
