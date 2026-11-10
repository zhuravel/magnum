package engine

// A PR controls its checkout's project config for the agent CLIs (.claude/,
// .mcp.json and the CLAUDE.md and AGENTS.md files for Claude Code), and an
// agent started with it loaded may
// reload it while it runs: Claude Code watches its settings files (hooks
// included) and skills, and loads a .claude/settings.json created later.
// agents keeps a changed config out of each launch and resume (the kind's
// project_untrust), so a running session must not see the checkout move
// under it: parkReloading quits those sessions first (DECISIONS "A PR that
// changes .claude/ or .mcp.json runs Claude with the user's settings only").

import (
	"context"
	"fmt"
	"maps"
	"slices"
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
// checkout must not move under it either. It returns the roles it quit, which a
// round.project_reload_parked event names, and the head it fetched for the
// decision (headProbe), which the checkout then checks out without fetching
// again ("" = none fetched).
func (e *Engine) parkReloading(ctx context.Context, job *roundJob) ([]string, string, error) {
	sessions, err := e.st.SessionsByPR(ctx, job.pr.ID)
	if err != nil {
		return nil, "", fmt.Errorf("sessions before the checkout: %w", err)
	}
	probe := &headProbe{e: e, job: job}
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
// out with Slots.CheckoutFetched), the PR's stored file list, git's list
// of the files the fetched head changes, and the links on the slot's disk.
type headProbe struct {
	e   *Engine
	job *roundJob

	fetchedOnce bool
	fetched     string // the head the fetch found ("" = not fetched, or it failed)
	filesOnce   bool
	files       store.PRFiles
	filesOK     bool
	namesOnce   bool
	names       []string // every file the fetched head changes since its merge base (git)
	namesErr    error
	disk        map[string]diskLinks // kind -> the links on the slot's disk (agents.ProjectLinks)
}

// diskLinks is what agents.ProjectLinks found on the slot's disk.
type diskLinks struct {
	targets []string
	partial bool
}

// leavesProjectAlone reports whether the head the checkout brings leaves
// the project config s's agent reloads as the base the session compared it
// with has it: then the session has nothing new to reload and need not be
// quit. The checkout's fetch runs first, so the head decided on is the one
// checked out, even when GitHub's moved since the radar read it. That base
// is the session's (agents.ProjectBase): the PR's base branch, or, for a
// PR based on another branch than the repository's default (stacked, or
// retargeted since), origin/<default branch>, since what the head brings
// from that branch's own .claude/ is not the team's. The PR's file list
// the poller stored decides when it is complete, lists that head and was
// read against the PR's base branch, the session's own; otherwise (a PR
// of more files than the list holds, a list of another head or base, none,
// a stacked PR) git does: every file the head changes since its merge base
// with that base (gitx.Client.ChangedNames, read once for every session),
// matched here like the list (agents.ProjectTouched) rather than by git's
// pathspec, whose icase folds ASCII only (a head's
// .agents/ſkills/pwn/SKILL.md, which macOS writes into .agents/skills/pwn/).
// Either way a file at or under a link's target counts, and so does a path
// on the way to one (linkTargets: .claude/skills -> ../.agents/skills
// loads what the head changes there, and .agentſ may become a link,
// agents.UnderTarget). A fetch that fails (or none, for a checkout not in
// place) leaves no head to decide on, while the checkout's own fetch may
// then bring a newer one than the radar's list is of; that, a session of
// an unknown kind, links that cannot all be read, a git that cannot tell
// and no answer at all are no proof, so the session is quit.
func (p *headProbe) leavesProjectAlone(ctx context.Context, s store.Session) bool {
	kind := deref(s.AgentKind)
	paths := agents.ProjectPaths(kind)
	if len(paths) == 0 {
		return false
	}
	head := p.fetch(ctx)
	if head == "" {
		return false
	}
	targets, ok := p.linkTargets(ctx, kind, head)
	if !ok {
		return false
	}
	touches := func(files []string) bool {
		return agents.ProjectTouched(kind, files) || slices.ContainsFunc(files, func(f string) bool {
			return slices.ContainsFunc(targets, func(t string) bool { return agents.UnderTarget(f, t) || agents.UnderTarget(t, f) })
		})
	}
	pr, own := p.job.pr, "origin/"+strings.TrimPrefix(prBase(p.job.repo, p.job.pr), "origin/")
	base := agents.ProjectBase(pr, p.job.repo, own)
	if f, ok := p.list(ctx); ok && !f.Truncated && f.HeadSHA == head && base == own && (f.BaseRef == "" || f.BaseRef == deref(pr.BaseRef)) {
		return !touches(f.Paths)
	}
	if p.e.d.Git == nil {
		return false
	}
	changed, err := p.changedNames(ctx, base, head)
	return err == nil && !touches(changed)
}

// changedNames are the files head changes since its merge base with base,
// read once (ChangedNames): a git that fails is logged once and quits
// every session that reloads its config.
func (p *headProbe) changedNames(ctx context.Context, base, head string) ([]string, error) {
	if !p.namesOnce {
		p.namesOnce = true
		p.names, p.namesErr = p.e.d.Git.ChangedNames(ctx, p.job.slot.Path, base, head)
		if p.namesErr != nil {
			p.e.log.Warn("project config: cannot compare the head with its merge base, so a session that reloads it is quit",
				"pr", p.job.pr.ID, "head", head, "err", p.namesErr)
		}
	}
	return p.names, p.namesErr
}

// linkTargets are where the links of kind's config lead: on the slot's
// disk (agents.ProjectLinks, the links the session loaded through, walked
// once per kind), and in the tree of head, the fetched one (configLinks,
// which also has the links below the root and those the base added
// since), each target once, sorted. false when they cannot all be read:
// the disk's walk stopped short, or the head's tree cannot be read.
func (p *headProbe) linkTargets(ctx context.Context, kind, head string) ([]string, bool) {
	d, ok := p.disk[kind]
	if !ok {
		d.targets, d.partial = agents.ProjectLinks(ctx, kind, p.job.slot.Path)
		if p.disk == nil {
			p.disk = map[string]diskLinks{}
		}
		p.disk[kind] = d
		if d.partial {
			p.e.log.Warn("project config: too many entries to look through for links, so a session that reloads it is quit", "pr", p.job.pr.ID)
		}
	}
	if d.partial {
		return nil, false
	}
	targets := slices.Clone(d.targets)
	if head != "" {
		pr := p.job.pr
		pr.HeadSHA = head
		tree, err := p.e.configLinks(ctx, pr)
		if err != nil {
			return nil, false
		}
		targets = append(targets, slices.Collect(maps.Keys(tree))...)
	}
	slices.Sort(targets)
	return slices.Compact(targets), true
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
		p.e.log.Warn("project config: fetch before the checkout failed, so a session that reloads the config is quit", "pr", p.job.pr.ID, "err", err)
		return ""
	}
	p.e.headFetched(sha)
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
