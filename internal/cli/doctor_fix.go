package cli

// The doctor's line of the fix loop's move into the factory (DECISIONS "The
// fix loop moves into the factory"): the PRs whose fix work of magnum's
// keeps them from handing over (the drain proof's morning list,
// engine.FixInFlight), and the PRs whose factory lease magnum could not
// read and reviewed without it (failed open, engine.FixLeaseUnreads).

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
)

// doctorFixHandover is the fix handover's lines: a WARN while a PR that
// hands over to the factory waits for its fix work to end, or while a read
// of the factory's lease failed (open: magnum reviews the PR without it),
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
	switch {
	case len(waiting) > 0:
		out = append(out, doctorWarned(name, fmt.Sprintf("%d PRs wait to hand over to the factory until magnum's fix work on them ends: %s",
			len(waiting), strings.Join(waiting, ", ")), "`magnum fix --drained` names your action for each"))
	case len(list) > 0:
		out = append(out, doctorOK(name, fmt.Sprintf("magnum's fix work is in flight on %d PRs (`magnum fix --drained` lists it)", len(list))))
	default:
		out = append(out, doctorOK(name, "none of magnum's fix work is in flight: every PR can hand over to the factory"))
	}
	if line, ok := doctorLeaseUnreads(ctx, d, unread); ok {
		out = append(out, line)
	}
	return out
}

// doctorLeaseUnreads is the line of the PRs whose factory lease magnum
// could not read: a WARN naming those it reviews without the lease (failed
// open) and those whose round waits for the grace; ok false for none.
func doctorLeaseUnreads(ctx context.Context, d doctorDeps, unread map[int64]engine.LeaseUnread) (doctorCheck, bool) {
	if len(unread) == 0 {
		return doctorCheck{}, false
	}
	var open, waits []string
	for _, id := range slices.Sorted(maps.Keys(unread)) {
		u := unread[id]
		label := fmt.Sprintf("PR %d", id)
		if pr, err := d.Store.PRByID(ctx, id); err == nil {
			if repo, err := d.Store.RepoByID(ctx, pr.RepoID); err == nil {
				label = actRefLabel(d.Config.Daemon.DefaultRepo, repo.FullName(), pr.Number)
			}
		}
		what := fmt.Sprintf("%s since %s (%s)", label, u.Since.Local().Format("Jan 2 15:04"), cmp.Or(actClean(u.Err), "no error"))
		if u.Open {
			open = append(open, what)
		} else {
			waits = append(waits, what)
		}
	}
	var parts []string
	if len(open) > 0 {
		parts = append(parts, fmt.Sprintf("magnum reviews %d PRs the factory fixes without its lease, which it cannot read (failed open): %s",
			len(open), strings.Join(open, ", ")))
	}
	if len(waits) > 0 {
		parts = append(parts, fmt.Sprintf("%d PRs' rounds wait for the factory's lease, which cannot be read: %s", len(waits), strings.Join(waits, ", ")))
	}
	return doctorWarned("factory lease", strings.Join(parts, "; "),
		"the factory's registry cannot be read; `magnum factory status` says whether its daemon runs"), true
}

// recordKinds are recs' kinds, in a few words.
func recordKinds(recs []engine.FixRecord) string {
	kinds := make([]string, 0, len(recs))
	for _, r := range recs {
		kinds = append(kinds, strings.ReplaceAll(r.Kind, "_", " "))
	}
	return strings.Join(kinds, ", ")
}
