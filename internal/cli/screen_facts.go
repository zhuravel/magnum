package cli

// What the titles of the dashboard and the PR board say about the daemon
// (tui.DaemonFacts): an older build running, a pause and the review requests
// it holds, a drain, the watches whose radar calls fail, the PRs magnum
// approved that wait for the operator's approval and those magnum approved
// as the operator, the Codex budget's
// pace when it reaches a cap before the window resets, and the notes
// curation proposals waiting for review.
// Both screens read them from the registry with every refresh, without
// launchctl or herdr.

import (
	"context"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// screenFacts reads the facts: the daemon's pid, build, drain and failing
// watches as `magnum status` reads them, the `magnum pause` with the
// requests it holds, and the Codex budget.
func screenFacts(ctx context.Context, d statusDeps) tui.DaemonFacts {
	var f tui.DaemonFacts
	if d.Store == nil {
		return f
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	d.Launchd = nil // no launchctl with every refresh: the titles do not show it
	r := statusReport{GeneratedAt: now}
	kv := statusKV{ctx: ctx, st: d.Store}
	statusGatherDaemon(ctx, d, kv, &r)
	statusGatherUsage(d, kv, &r)

	if dm := r.Daemon; dm.Skew != "" && dm.Build != nil {
		f.SkewOld, f.SkewSince, f.SkewNew = dm.Build.Label(), dm.Build.StartedAt, screenSkewNew(*dm.Build, d.Version, d.ModTime)
	}
	if r.Daemon.DrainingSince != nil {
		f.Draining, f.DrainerPID = true, r.Daemon.DrainerPID
	}
	f.PollsFailing = screenPollsFailing(r.Daemon.PollsFailing)
	if v, _ := kv.get(store.KVDaemonPaused); v == "1" {
		f.Paused, f.Held = true, store.Deref(kv.getInt(engine.KVDaemonPausedHeld))
		if at := kv.getTime(engine.KVDaemonPausedAt); at != nil {
			f.PausedSince = *at
		}
	}
	if c := r.Codex; c != nil {
		f.Codex = screenCodexPace(*c)
	}
	if n, err := d.Store.CountNotesProposals(ctx, store.ProposalPending); err == nil {
		f.NotesProposals = n
	}
	approved := map[int64]bool{} // approved as the operator: they are not needed
	if list, err := d.Store.StandingAutoApprovals(ctx); err == nil {
		f.AutoApproved = len(list)
		for _, a := range list {
			approved[a.PRID] = true
		}
	}
	if list, err := d.Store.NeedsMePRs(ctx, d.Config.CommentsWhenClean, d.Config.SelfMatch()); err == nil {
		for _, n := range list {
			if !approved[n.PR.ID] {
				f.NeedsMe++
			}
		}
	}
	return f
}

// screenPollsFailing is the watches whose radar calls fail as the screens
// take them (statusPollsFailing chose and ordered them); nil when none.
func screenPollsFailing(fs []statusPollFailing) []tui.WatchFailing {
	var out []tui.WatchFailing
	for _, f := range fs {
		out = append(out, tui.WatchFailing{Watch: f.Watch, Since: f.Since, Error: f.Error})
	}
	return out
}

// screenSkewNew is what is built that the daemon does not run, as the titles
// say it: "v1.5.0 built" when this binary's stamped version differs (as
// engine.SkewNote compares them), else "new build Oct 5 18:48" for the binary
// on disk.
func screenSkewNew(b engine.Build, version string, modTime func(string) (time.Time, bool)) string {
	stamped := func(v string) bool { return v != "" && v != "dev" }
	if stamped(version) && stamped(b.Version) && version != b.Version {
		return version + " built"
	}
	if modTime == nil {
		modTime = engine.FileModTime
	}
	if b.Path != "" {
		if mt, ok := modTime(b.Path); ok {
			return "new build " + mt.Local().Format("Jan 2 15:04")
		}
	}
	return "a new build"
}

// screenCodexPace is the Codex budget's pace for the titles: when, at the
// pace it has been spent since its window began, it reaches codex_soft (or
// codex_hard, once past the soft cap) before the window resets
// (statusGatherUsage projects both); nil when it does not, or the window is
// unknown.
func screenCodexPace(u statusCodexUsage) *tui.CodexPace {
	switch pct := float64(u.Percent); {
	case u.SoftAt != nil:
		return &tui.CodexPace{Used: u.Percent, Cap: u.Soft, At: *u.SoftAt}
	case u.HardAt != nil && (u.Soft <= 0 || pct >= u.Soft):
		return &tui.CodexPace{Used: u.Percent, Cap: u.Hard, At: *u.HardAt}
	}
	return nil // the first cap ahead comes after the reset: the next one too
}
