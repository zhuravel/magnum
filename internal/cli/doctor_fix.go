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
	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/textx"
)

// doctorFixHandover is the fix handover's lines: a WARN while a PR that
// hands over to the factory waits for its fix work to end, while PRs are
// the factory's and it fixes none (doctorFactorysPRs), while a handover
// waits for [factory] fix = "on" (doctorHandoverKey), or while a read of
// the factory's lease or of an owner row failed
// (doctorLeaseUnreads), and a PASS that counts magnum's fix work in flight
// otherwise.
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
	// The drain proof answers under either key; the handover itself waits
	// for it (C11, as the handover's dry run says).
	refused := ""
	if !d.Config.Factory.Fixes() {
		refused = fmt.Sprintf("; the handover itself is refused while [factory] fix is %q", cmp.Or(d.Config.Factory.Fix, "off"))
	}
	// While the factory fixes none, the PRs it owns have no fixer (C8): no
	// line says that every PR can hand over.
	ownedLine, fixesNone := doctorFactorysPRs(ctx, d)
	canHand := ": every PR can hand over to the factory"
	if fixesNone {
		canHand = ""
	}
	switch {
	case len(waiting) > 0:
		out = append(out, doctorWarned(name, fmt.Sprintf("%d PRs wait to hand over to the factory until magnum's fix work on them ends: %s%s",
			len(waiting), strings.Join(waiting, ", "), held), "`magnum fix --drained` names your action for each"))
	case len(list) > 0:
		out = append(out, doctorOK(name, fmt.Sprintf("magnum's fix work is in flight on %d PRs (`magnum fix --drained` lists it)%s%s", len(list), held, refused)))
	default:
		out = append(out, doctorOK(name, "none of magnum's fix work is in flight"+canHand+held+refused))
	}
	if fixesNone {
		out = append(out, ownedLine)
	}
	if c, ok := doctorHandoverKey(ctx, d); ok {
		out = append(out, c)
	}
	return append(out, doctorLeaseUnreads(ctx, d, unread)...)
}

// doctorFactorysPRs is the WARN of the PRs whose owner row names the factory
// or "returning" while [factory] fix is not "on" (C8): magnum starts no
// fixer on them, and the factory fixes none, so nothing fixes them until
// the way back returns those the factory has not worked, or the operator
// fixes them by hand. ok false: the key is on, or no such PR. A row that
// does not parse is the owner row line's (doctorLeaseUnreads).
func doctorFactorysPRs(ctx context.Context, d doctorDeps) (doctorCheck, bool) {
	if d.Config.Factory.Fixes() {
		return doctorCheck{}, false
	}
	key := fmt.Sprintf("[factory] fix is %q", cmp.Or(d.Config.Factory.Fix, "off"))
	vals, err := d.Store.KVValues(ctx, "pr.*.fix_owner")
	if err != nil {
		return doctorWarned("fix handover", "the PRs' owner rows cannot be read: "+err.Error(), ""), true
	}
	var prs []string
	for k, raw := range vals {
		var id int64
		if _, err := fmt.Sscanf(k, "pr.%d.", &id); err != nil || fixwork.KVPRFixOwner(id) != k {
			continue
		}
		o, err := fixwork.ParseFixOwner(raw)
		if err != nil {
			continue
		}
		label := fmt.Sprintf("PR %d", id)
		if pr, err := d.Store.PRByID(ctx, id); err == nil {
			if repo, err := d.Store.RepoByID(ctx, pr.RepoID); err == nil {
				label = actRefLabel(d.Config.Daemon.DefaultRepo, repo.FullName(), pr.Number)
			}
		}
		if !o.Factory() {
			label += " (returning)"
		}
		prs = append(prs, label)
	}
	if len(prs) == 0 {
		return doctorCheck{}, false
	}
	slices.Sort(prs)
	// While --handover all stands, the daemon refuses every `back <pr>`
	// (returnToMagnum): `back all` lifts it first (C4).
	back := "`magnum fix --handover back <pr>` returns one"
	if _, all, err := d.Store.GetKV(ctx, engine.KVFixHandoverAll); all && err == nil {
		back = "`magnum fix --handover back all` lifts `magnum fix --handover all` and returns each"
	}
	return doctorWarned("fix handover", fmt.Sprintf("%d PRs are the factory's, which fixes none while %s: %s", len(prs), key, strings.Join(prs, ", ")),
		back+" the factory has not worked since it took it (a returning PR goes back by itself at a tick "+
			"that finds the factory still idle on it); fix the rest by hand, or set [factory] fix = \"on\" and restart both daemons"), true
}

// doctorHandoverKey is the WARN of the handovers that wait for [factory]
// fix = "on" (the engine owner's ruling on R3): `magnum fix --handover all`
// or one PR's, written while the key was on, flips no PR while it is off,
// and magnum fixes those PRs meanwhile. ok false: the key is on, or no
// handover waits.
func doctorHandoverKey(ctx context.Context, d doctorDeps) (doctorCheck, bool) {
	if d.Config.Factory.Fixes() {
		return doctorCheck{}, false
	}
	_, all, err := d.Store.GetKV(ctx, engine.KVFixHandoverAll)
	all = all && err == nil
	keys, _ := d.Store.KVKeys(ctx, "pr.*.fix_handover")
	var prs []string
	for _, key := range keys {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.", &id); err != nil || engine.KVPRFixHandover(id) != key {
			continue
		}
		label := fmt.Sprintf("PR %d", id)
		if pr, err := d.Store.PRByID(ctx, id); err == nil {
			if repo, err := d.Store.RepoByID(ctx, pr.RepoID); err == nil {
				label = actRefLabel(d.Config.Daemon.DefaultRepo, repo.FullName(), pr.Number)
			}
		}
		prs = append(prs, label)
	}
	slices.Sort(prs)
	handovers := "the handover of " + strings.Join(prs, ", ")
	if len(prs) > 1 {
		handovers = "the handovers of " + strings.Join(prs, ", ")
	}
	const key = `[factory] fix = "on"`
	var what string
	switch {
	case all && len(prs) > 0:
		what = "`magnum fix --handover all` waits for " + key + ", and so does " + handovers
	case all:
		what = "`magnum fix --handover all` waits for " + key
	case len(prs) > 0:
		what = handovers + " " + textx.Plural(len(prs), "waits", "wait") + " for " + key
	default:
		return doctorCheck{}, false
	}
	return doctorWarned("fix handover", what+": no PR flips to the factory, and magnum fixes them meanwhile",
		"set "+key+" and restart both daemons to hand them over, or `magnum fix --handover back all` calls them off"), true
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
