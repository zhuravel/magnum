package engine

import (
	"context"
	"fmt"

	"github.com/zhuravel/magnum/internal/config"
)

// rerunRoles names the runs = "first" roles with rerun_min_lines whose code
// changed enough since the head of their last completed run: they run again
// this round, as if requested. The measure is the re-review threshold's
// (MeasureDelta: code lines, not comments, blank lines, whitespace moves or
// documentation), from one compare call per role as the watch's poll
// identity. A role that never completed a run is runs = "first"'s business;
// any failure skips the rerun (the role stays on request).
func (e *Engine) rerunRoles(ctx context.Context, job *roundJob, roles []config.Role, target string) []string {
	if e.d.GitHub == nil || target == "" {
		return nil
	}
	var out []string
	for _, r := range roles {
		if r.Runs != config.RunsFirst || r.RerunMinLines <= 0 {
			continue
		}
		last, err := e.st.LastRoleRunHead(ctx, job.pr.ID, r.Name)
		if err != nil || last == "" || last == target {
			continue
		}
		gh := e.d.GitHub(job.watch.PollIdentity)
		if gh == nil {
			continue
		}
		deltas, err := gh.CompareFiles(ctx, job.repo.Owner, job.repo.Name, last, target)
		if err != nil {
			e.log.Info("rerun: compare failed; the role stays on request", "role", r.Name, "pr", job.pr.ID, "err", err)
			continue
		}
		size := MeasureDelta(deltas)
		if size.Lines < r.RerunMinLines {
			continue
		}
		out = append(out, r.Name)
		e.event(ctx, "info", prSubject(job.repo, job.pr.Number), "round.rerun_role",
			fmt.Sprintf("%s runs again: %d code lines changed since its last run at %s (rerun_min_lines %d)",
				r.Name, size.Lines, short(last), r.RerunMinLines),
			map[string]any{"role": r.Name, "lines": size.Lines, "since": last, "min": r.RerunMinLines})
	}
	return out
}
