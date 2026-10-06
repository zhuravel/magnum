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
// round retries uncharged) and so is one Quit cannot stop: the checkout must
// not move under it. It returns the roles it quit, which a
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
		switch st := herdr.Status(deref(s.AgentStatus)); st {
		case herdr.StatusWorking, herdr.StatusBlocked:
			return quit, fmt.Errorf("%s is %s and reloads the checkout's project config, so the checkout waits: %w", s.Role, st, agents.ErrBusy)
		}
		if err := e.d.Agents.Quit(ctx, s); err != nil {
			return quit, fmt.Errorf("quit %s before the checkout moves: %w", s.Role, err)
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
