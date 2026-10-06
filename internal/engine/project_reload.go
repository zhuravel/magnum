package engine

// A PR controls its checkout's project config for the agent CLIs (.claude/
// and .mcp.json for Claude Code), and an agent started with it loaded may
// reload it while it runs: Claude Code watches its settings files (hooks
// included) and skills, and loads a .claude/settings.json created later.
// agents keeps a changed config out of each launch and resume (the kind's
// project_untrust), so a running session must not see the checkout move
// under it: parkReloading quits those sessions first (DECISIONS "A PR that
// changes .claude/ or .mcp.json runs Claude with the user's settings only").

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// parkReloading quits, before the checkout moves to another commit, the
// PR's live sessions whose agent reloads the checkout's project config it
// started with (agents.Manager.ReloadsProject), parking their
// conversations: what that commit brings to the config never reaches a
// running session, and each one's next start, a resume, decides with the
// new head whether the config loads. An agent that works or is blocked
// cannot be quit safely, so that is an error wrapping agents.ErrBusy (the
// round retries uncharged); one Quit cannot stop fails the round's setup
// (charged: it backs off and asks for attention after its attempts), as the
// checkout must not move under it either. It returns the roles it quit, which a
// round.project_reload_parked event names.
func (e *Engine) parkReloading(ctx context.Context, job *roundJob) ([]string, error) {
	sessions, err := e.st.SessionsByPR(ctx, job.pr.ID)
	if err != nil {
		return nil, fmt.Errorf("sessions before the checkout: %w", err)
	}
	var quit []string
	for _, s := range sessions {
		if s.State != store.SessionLive || deref(s.AgentName) == "" || !e.d.Agents.ReloadsProject(ctx, s) {
			continue
		}
		if e.leavesProjectAlone(ctx, job, s) {
			continue
		}
		switch st := herdr.Status(deref(s.AgentStatus)); st {
		case herdr.StatusWorking, herdr.StatusBlocked:
			return quit, fmt.Errorf("%s is %s and reloads the checkout's project config, so the checkout waits: %w", s.Role, st, agents.ErrBusy)
		}
		if err := e.d.Agents.Quit(ctx, s); err != nil {
			// Not agents.ErrBusy: the agent was idle, so a quit that does not
			// stop it (its MCP servers still in the pane's foreground) will not
			// stop it on the next tick either. Charged, the round backs off and
			// the PR needs attention after its attempts instead of retrying
			// every tick for good.
			return quit, fmt.Errorf("quit %s before the checkout moves: %s", s.Role, err)
		}
		quit = append(quit, s.Role)
	}
	if len(quit) > 0 {
		e.event(ctx, "info", prSubject(job.repo, job.pr.Number), "round.project_reload_parked",
			strings.Join(quit, ", ")+" parked before the checkout moves: its agent reloads the checkout's project config while it runs, "+
				"so it resumes on the new head with what that head allows", map[string]any{"roles": quit})
	}
	return quit, nil
}

// leavesProjectAlone reports whether the head the round checks out leaves the
// project config s's agent reloads as the merge base has it, by the PR's
// file list the poller stored for that head (store.PRFiles): then the
// session has nothing new to reload and need not be quit. A list for
// another head, a cut-off one, none, or a session of an unknown kind is
// not proof, so the session is quit as before.
func (e *Engine) leavesProjectAlone(ctx context.Context, job *roundJob, s store.Session) bool {
	kind := deref(s.AgentKind)
	if kind == "" {
		return false
	}
	f, ok, err := e.st.PRFilesOf(ctx, job.pr.ID)
	if err != nil || !ok || f.Truncated || f.HeadSHA != cmp.Or(job.target, job.pr.HeadSHA) {
		return false
	}
	return !agents.ProjectTouched(kind, f.Paths)
}
