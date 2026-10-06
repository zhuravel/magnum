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
	"github.com/zhuravel/magnum/internal/config"
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
// checkout must not move under it either. target is the head the checkout
// is asked for. It returns the roles it quit, which a
// round.project_reload_parked event names, and the head it fetched for the
// decision (headProbe), which the checkout then checks out without fetching
// again ("" = none fetched).
func (e *Engine) parkReloading(ctx context.Context, job *roundJob, target string) ([]string, string, error) {
	sessions, err := e.st.SessionsByPR(ctx, job.pr.ID)
	if err != nil {
		return nil, "", fmt.Errorf("sessions before the checkout: %w", err)
	}
	probe := &headProbe{e: e, job: job, target: target}
	var quit []string
	for _, s := range sessions {
		if s.State != store.SessionLive || deref(s.AgentName) == "" || !e.d.Agents.ReloadsProject(ctx, s) {
			continue
		}
		if probe.leavesProjectAlone(ctx, s) {
			continue
		}
		switch st := herdr.Status(deref(s.AgentStatus)); st {
		case herdr.StatusWorking, herdr.StatusBlocked:
			return quit, probe.fetched, fmt.Errorf("%s is %s and reloads the checkout's project config, so the checkout waits: %w", s.Role, st, agents.ErrBusy)
		}
		if err := e.d.Agents.Quit(ctx, s); err != nil {
			// Not agents.ErrBusy: the agent was idle, so a quit that does not
			// stop it (its MCP servers still in the pane's foreground) will not
			// stop it on the next tick either. Charged, the round backs off and
			// the PR needs attention after its attempts instead of retrying
			// every tick for good.
			return quit, probe.fetched, fmt.Errorf("quit %s before the checkout moves: %s", s.Role, err)
		}
		quit = append(quit, s.Role)
	}
	if len(quit) > 0 {
		e.event(ctx, "info", prSubject(job.repo, job.pr.Number), "round.project_reload_parked",
			strings.Join(quit, ", ")+" parked before the checkout moves: its agent reloads the checkout's project config while it runs, "+
				"so it resumes on the new head with what that head allows", map[string]any{"roles": quit})
	}
	return quit, probe.fetched, nil
}

// headProbe answers, for one checkout, whether the head it brings changes
// the project config a session's agent reloads, reading each source once:
// the checkout's fetch (Slots.Fetch, whose head the checkout then checks
// out with Slots.CheckoutFetched), the PR's stored file list, and git per
// kind.
type headProbe struct {
	e      *Engine
	job    *roundJob
	target string // the head the checkout is asked for (the radar's)

	fetchedOnce bool
	fetched     string // the head the fetch found ("" = not fetched, or it failed)
	filesOnce   bool
	files       store.PRFiles
	filesOK     bool
	byKind      map[string]bool // kind -> the fetched head changes its config (or git could not tell)
}

// leavesProjectAlone reports whether the head the checkout brings leaves
// the project config s's agent reloads as the merge base has it: then the
// session has nothing new to reload and need not be quit. The checkout's
// fetch runs first, so the head decided on is the one checked out, even
// when GitHub's moved since the radar read it. The PR's file list the
// poller stored decides when it is complete and lists that head
// (store.PRFiles); otherwise (a PR of more files than the list holds, a
// list of another head, none) git does: the files the head changes at or
// under the kind's paths since its merge base with the PR's base branch
// (gitx.Client.ChangedUnder). A fetch that fails leaves the list of the
// radar's head; a session of an unknown kind, a git that cannot tell and
// no answer at all are no proof, so the session is quit.
func (p *headProbe) leavesProjectAlone(ctx context.Context, s store.Session) bool {
	kind := deref(s.AgentKind)
	paths := agents.ProjectPaths(kind)
	if len(paths) == 0 {
		return false
	}
	head := p.fetch(ctx)
	if f, ok := p.list(ctx); ok && !f.Truncated && f.HeadSHA == cmp.Or(head, p.target) {
		return !agents.ProjectTouched(kind, f.Paths)
	}
	if head == "" || p.e.d.Git == nil {
		return false
	}
	touched, ok := p.byKind[kind]
	if !ok {
		base := "origin/" + strings.TrimPrefix(prBase(p.job.repo, p.job.pr), "origin/")
		changed, err := p.e.d.Git.ChangedUnder(ctx, p.job.slot.Path, base, head, paths...)
		if err != nil {
			p.e.log.Warn("project config: cannot compare the head with its merge base, so a session that reloads it is quit",
				"pr", p.job.pr.ID, "head", head, "err", err)
		}
		touched = err != nil || len(changed) > 0
		if p.byKind == nil {
			p.byKind = map[string]bool{}
		}
		p.byKind[kind] = touched
	}
	return !touched
}

// fetch runs the checkout's fetch once, into the round's slot when the
// checkout will check the head out there (checksOutInPlace), and returns
// the head it fetched ("" when it did not or could not).
func (p *headProbe) fetch(ctx context.Context) string {
	if p.fetchedOnce {
		return p.fetched
	}
	p.fetchedOnce = true
	if !p.job.checksOutInPlace() || p.e.d.Slots == nil {
		return ""
	}
	var pool config.Pool
	if p.job.pool != nil {
		pool = *p.job.pool
	}
	sha, err := p.e.d.Slots.Fetch(ctx, p.job.slot, p.job.pr, pool)
	if err != nil {
		p.e.log.Warn("project config: fetch before the checkout", "pr", p.job.pr.ID, "err", err)
		return ""
	}
	p.fetched = sha
	return sha
}

// list is the PR's stored file list, read once.
func (p *headProbe) list(ctx context.Context) (store.PRFiles, bool) {
	if !p.filesOnce {
		p.filesOnce = true
		f, ok, err := p.e.st.PRFilesOf(ctx, p.job.pr.ID)
		p.files, p.filesOK = f, ok && err == nil
	}
	return p.files, p.filesOK
}
