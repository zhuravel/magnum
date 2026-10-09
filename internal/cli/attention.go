package cli

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const attentionUsage = "attention [--list] [--no-reveal] [--new-window] [--json]"

type attentionOpts struct {
	list, noReveal, newWindow, json bool
}

// attentionItem is one thing that needs the user, most urgent tier first.
type attentionItem struct {
	Tier   int       `json:"-"`
	Kind   string    `json:"kind"` // blocked | needs_attention | failed | done
	PR     string    `json:"pr"`
	URL    string    `json:"url,omitempty"`
	Role   string    `json:"role,omitempty"`
	Agent  string    `json:"agent,omitempty"`
	PaneID string    `json:"pane_id,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	Detail string    `json:"detail,omitempty"`
	// Fix is the next step for a PR in needs_attention (attention.Reason.Fix).
	Fix string `json:"fix,omitempty"`

	session *store.Session
}

const (
	attentionBlocked = iota + 1
	attentionNeeds
	attentionFailed
	attentionDone
)

var attentionKinds = map[int]string{attentionBlocked: "blocked", attentionNeeds: "needs_attention", attentionFailed: "failed", attentionDone: "done"}

func newAttentionCmd(c *Context) *cobra.Command {
	var o attentionOpts
	cmd := newCommand(groupAct, attentionUsage, "jump to the review pane that needs you (blocked, needs attention, failed, done)",
		"Jump to the review pane that needs you most: a blocked agent first, then a PR that needs attention, "+
			"a failed round, then a finished one. --list prints everything that needs you instead. When nothing "+
			"does it exits 1, so the herdr plugin action shows the message as a toast.",
		func(pos []string) int { return runAttention(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.list, "list", false, "list everything that needs you instead of jumping")
	fs.BoolVar(&o.noReveal, "no-reveal", false, "only focus inside herdr; do not bring the terminal to the front")
	fs.BoolVar(&o.newWindow, "new-window", false, "open a new terminal window (not a tab) when no herdr client is found")
	fs.BoolVar(&o.json, "json", false, "print as JSON")
	return cmd
}

func runAttention(c *Context, o attentionOpts, pos []string) int {
	if len(pos) > 0 {
		return actUsage(c, "attention", "takes no arguments", attentionUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "attention", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return attentionMain(ctx, c, d, o)
}

// attentionMain focuses the most urgent item (exit 0), or prints "nothing
// needs you" (exit 1, so the herdr plugin action shows it as a toast).
func attentionMain(ctx context.Context, c *Context, d *actDeps, o attentionOpts) int {
	items, err := attentionCollect(ctx, d)
	if err != nil {
		return cmdFail(c, "attention", err)
	}
	if o.list {
		if o.json {
			if items == nil {
				items = []attentionItem{}
			}
			_ = writeJSON(c.Stdout, items)
			return 0
		}
		if len(items) == 0 {
			fmt.Fprintln(c.Stdout, "nothing needs you")
			return 0
		}
		rows := make([][]string, 0, len(items))
		for _, it := range items {
			where := it.Agent
			if where == "" {
				where = "(no live pane)"
			}
			rows = append(rows, []string{it.Kind, it.PR, actRoleName(it.Role), where, actAgo(d.now(), it.Since), textx.Clip(actClean(it.Detail), 120)})
		}
		actTable(c.Stdout, []string{"WHY", "PR", "ROLE", "AGENT", "SINCE", "DETAIL"}, rows)
		return 0
	}
	for _, it := range items {
		if it.session == nil {
			continue
		}
		res := actFocusResult{PR: it.PR, URL: it.URL, Role: it.Role, Reason: it.Kind}
		if err := d.focus(ctx, &res, *it.session, o.noReveal, o.newWindow); err != nil {
			if !attentionGone(err) {
				return cmdFail(c, "attention", err)
			}
			// A stale pane or agent name must not hide the others.
			fmt.Fprintf(c.Stderr, "magnum attention: skipped %s %s: %v\n", it.PR, actRoleName(it.Role), err)
			continue
		}
		if o.json {
			_ = writeJSON(c.Stdout, res)
		} else {
			fmt.Fprintln(c.Stdout, actFocusLine(res))
			if it.Detail != "" {
				fmt.Fprintln(c.Stdout, "  "+textx.Clip(actClean(it.Detail), 200))
			}
			if it.Fix != "" {
				fmt.Fprintln(c.Stdout, "  fix: "+actClean(it.Fix))
			}
			if more := len(items) - 1; more > 0 {
				fmt.Fprintf(c.Stdout, "  %d more: `magnum attention --list`\n", more)
			}
		}
		return 0
	}
	if len(items) == 0 {
		fmt.Fprintln(c.Stdout, "nothing needs you")
		return 1
	}
	// Only items without a live pane (parked PRs needing attention).
	for _, it := range items {
		fmt.Fprintf(c.Stdout, "%s %s: %s (no live pane: `magnum open %s`)\n", it.PR, it.Kind, textx.Clip(actClean(it.Detail), 200), it.PR)
		if it.Fix != "" {
			fmt.Fprintln(c.Stdout, "  fix: "+actClean(it.Fix))
		}
	}
	return 1
}

// attentionGone reports a focus that failed because the session's pane or
// agent no longer exists in herdr (the next item is tried instead).
func attentionGone(err error) bool {
	return herdr.IsCode(err, herdr.CodeAgentNotFound) || herdr.IsCode(err, herdr.CodePaneNotFound)
}

// attentionCollect gathers what needs the user from herdr's live agent
// statuses and the registry: blocked agents, PRs in needs_attention, PRs
// whose latest round failed, and finished agents not looked at yet (herdr
// status done).
func attentionCollect(ctx context.Context, d *actDeps) ([]attentionItem, error) {
	snap, err := d.Herdr.Snapshot(ctx)
	if err != nil {
		return nil, d.herdrErr(err)
	}
	live, err := d.Store.LiveSessions(ctx)
	if err != nil {
		return nil, err
	}
	targets := map[int64]actTarget{}
	target := func(prID int64) (actTarget, bool) {
		if t, ok := targets[prID]; ok {
			return t, t.hasPR()
		}
		t, err := d.targetByPRID(ctx, prID)
		targets[prID] = t
		return t, err == nil
	}
	byPR := map[int64][]store.Session{}
	var items []attentionItem
	for i := range live {
		s := live[i]
		byPR[s.PRID] = append(byPR[s.PRID], s)
		ag, ok := attentionAgent(snap, s)
		if !ok {
			continue
		}
		tier := 0
		switch ag.AgentStatus {
		case herdr.StatusBlocked:
			tier = attentionBlocked
		case herdr.StatusDone:
			tier = attentionDone
		default:
			continue
		}
		t, ok := target(s.PRID)
		if !ok {
			continue
		}
		since := store.Deref(s.AgentStatusAt)
		if since.IsZero() {
			since = s.StartedAt
		}
		items = append(items, attentionItem{Tier: tier, PR: d.actLabel(t.full(), t.PR.Number), URL: t.PR.URL, Role: s.Role,
			Agent: store.Deref(s.AgentName), PaneID: store.Deref(s.HerdrPaneID), Since: since, session: &live[i]})
	}

	needs, err := d.Store.ListPRs(ctx, store.PRFilter{States: []string{store.PRNeedsAttention}})
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for _, pr := range needs {
		seen[pr.ID] = true
		t, ok := target(pr.ID)
		if !ok {
			continue
		}
		it := attentionItem{Tier: attentionNeeds, PR: d.actLabel(t.full(), pr.Number), URL: pr.URL, Role: actJudgeFor(d.Cfg, t.full()),
			Since: pr.UpdatedAt, Detail: store.Deref(pr.LastError)}
		if why, ok := prAttention(ctx, d.Store, pr, it.PR); ok {
			it.Detail, it.Fix = why.Summary, why.Fix
		}
		if s := attentionPick(d.Cfg, byPR[pr.ID]); s != nil {
			it.Role, it.Agent, it.PaneID, it.session = s.Role, store.Deref(s.AgentName), store.Deref(s.HerdrPaneID), s
		}
		items = append(items, it)
	}

	// Failed: the latest round of a PR with live sessions ended failed and
	// nothing newer is running.
	quiet := append(slices.Clone(prInFlight), store.PRNeedsAttention)
	for prID, sessions := range byPR {
		if seen[prID] {
			continue
		}
		t, ok := target(prID)
		if !ok || slices.Contains(quiet, t.PR.State) {
			continue
		}
		runs, err := d.Store.RunsByPR(ctx, prID)
		if err != nil || len(runs) == 0 {
			continue
		}
		judge := latestRoundJudge(d.Cfg, runs)
		if judge == nil || judge.State != store.RunFailed {
			continue
		}
		detail := store.Deref(judge.Outcome)
		if e := store.Deref(judge.Error); e != "" {
			detail += ": " + e
		}
		since := store.Deref(judge.EndedAt)
		if since.IsZero() {
			since = judge.CreatedAt
		}
		it := attentionItem{Tier: attentionFailed, PR: d.actLabel(t.full(), t.PR.Number), URL: t.PR.URL, Role: judge.Role,
			Since: since, Detail: detail}
		if s := attentionPick(d.Cfg, sessions); s != nil {
			it.Role, it.Agent, it.PaneID, it.session = s.Role, store.Deref(s.AgentName), store.Deref(s.HerdrPaneID), s
		}
		items = append(items, it)
	}

	// One item per pane: the most urgent tier wins.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Tier != items[j].Tier {
			return items[i].Tier < items[j].Tier
		}
		if !items[i].Since.Equal(items[j].Since) {
			return items[i].Since.Before(items[j].Since)
		}
		return items[i].PR < items[j].PR
	})
	out := items[:0]
	panes := map[string]bool{}
	for _, it := range items {
		if it.PaneID != "" {
			if panes[it.PaneID] {
				continue
			}
			panes[it.PaneID] = true
		}
		it.Kind = attentionKinds[it.Tier]
		out = append(out, it)
	}
	return out, nil
}

// attentionAgent finds the session's agent in the snapshot.
func attentionAgent(snap herdr.Snapshot, s store.Session) (herdr.AgentInfo, bool) {
	if name := store.Deref(s.AgentName); name != "" {
		if a, ok := snap.AgentByName(name); ok {
			return a, true
		}
	}
	if pane := store.Deref(s.HerdrPaneID); pane != "" {
		for _, a := range snap.Agents {
			if a.PaneID == pane {
				return a, true
			}
		}
	}
	return herdr.AgentInfo{}, false
}

// latestRoundJudge is the judge run of runs' latest round (the highest
// round number), whose state says how the round ended: the newest judge run
// of that round that is not its own pass (kind own_pass, prompted with the
// reviewers); nil when there is none.
func latestRoundJudge(cfg *config.Config, runs []store.Run) *store.Run {
	top := 0
	for _, r := range runs {
		top = max(top, r.Round)
	}
	var judge *store.Run
	for i := range runs {
		r := &runs[i]
		if r.Round == top && r.Kind != store.RunOwnPass && actIsJudge(cfg, r.Role) && (judge == nil || !r.CreatedAt.Before(judge.CreatedAt)) {
			judge = r
		}
	}
	return judge
}

// attentionPick prefers the judge session with a pane, then any with a pane.
func attentionPick(cfg *config.Config, sessions []store.Session) *store.Session {
	var first *store.Session
	for i := range sessions {
		s := &sessions[i]
		if store.Deref(s.HerdrPaneID) == "" && store.Deref(s.AgentName) == "" {
			continue
		}
		if actIsJudge(cfg, s.Role) {
			return s
		}
		if first == nil {
			first = s
		}
	}
	return first
}
