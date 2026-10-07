package cli

// `magnum status --watch` on a terminal: the live dashboard of internal/tui,
// fed by statusGather and acting through the act commands' own code paths
// (screenActions).

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// statusRefresh is how often the dashboard (and the plain --watch) redraws.
const statusRefresh = 2 * time.Second

// statusDashboard runs the dashboard (and the PR board, which tab switches
// to) until the user quits or ctx ends.
func statusDashboard(ctx context.Context, c *Context, d statusDeps, o statusOptions) int {
	// tab switches to the PR board (open PRs of every repository) and back.
	po := prsOptions{Sort: tui.SortUpdated, Desc: true}
	if err := runInspScreens(ctx, c, d, o, po, screenDashboard); err != nil {
		return cmdFail(c, "status", err)
	}
	return 0
}

// kvColumnWidths keeps the column widths dragged on the screens in the
// registry's kv table (store.KVScreenWidths), as JSON.
type kvColumnWidths struct{ st *store.Store }

// LoadWidths reads a screen's kept widths; nil when none are kept.
func (k kvColumnWidths) LoadWidths(ctx context.Context, screen string) (map[string]int, error) {
	v, ok, err := k.st.GetKV(ctx, store.KVScreenWidths(screen))
	if err != nil || !ok {
		return nil, err
	}
	var w map[string]int
	if err := json.Unmarshal([]byte(v), &w); err != nil {
		return nil, fmt.Errorf("column widths of the %s: %w", screen, err)
	}
	return w, nil
}

// SaveWidths keeps a screen's widths; an empty map forgets them.
func (k kvColumnWidths) SaveWidths(ctx context.Context, screen string, w map[string]int) error {
	key := store.KVScreenWidths(screen)
	if len(w) == 0 {
		return k.st.DeleteKV(ctx, key)
	}
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return k.st.SetKV(ctx, key, string(b))
}

// statusDashSource gathers one dashboard snapshot per refresh. Refreshes never
// ask GitHub for the manual worktrees' PR states: with --all that would spend
// rate budget every 2s.
func statusDashSource(d statusDeps, o statusOptions) tui.SourceFunc {
	o.NoGitHub = true
	defRepo := ""
	if d.Config != nil {
		defRepo = d.Config.Daemon.DefaultRepo
	}
	return func(ctx context.Context) (tui.StatusData, error) {
		r, err := statusGather(ctx, d, o)
		if err != nil {
			return tui.StatusData{}, err
		}
		data := statusDashData(r, defRepo)
		data.Rounds.Progress = statusDashProgress(ctx, d, data.Rounds.ActivePRs)
		data.Facts = screenFacts(ctx, d)
		return data, nil
	}
}

// statusDashProgress is the progress of the round each label names (the
// report's rounds: "talkable#11940", " (delta check)" after a delta
// check's), read as the board reads it (roundProgressOf); nil for a label
// that no round in flight, or more than one, answers to. Three queries
// whatever the number of rounds: the PRs in flight, the repositories and
// the rounds' runs. A registry that cannot say leaves the progress out
// rather than the dashboard.
func statusDashProgress(ctx context.Context, d statusDeps, labels []string) []*tui.RoundProgress {
	if len(labels) == 0 || d.Store == nil {
		return nil
	}
	prs, err := d.Store.ListPRs(ctx, store.PRFilter{States: roundRunningStates})
	if err != nil || len(prs) == 0 {
		return nil
	}
	repos, err := d.Store.ListRepos(ctx)
	if err != nil {
		return nil
	}
	cfg := d.Config
	if cfg == nil {
		cfg = config.Defaults()
	}
	progress, err := roundProgressOf(ctx, d.Store, cfg, prs)
	if err != nil || len(progress) == 0 {
		return nil
	}
	repoName := make(map[int64]string, len(repos))
	for _, rp := range repos {
		repoName[rp.ID] = rp.FullName()
	}
	out := make([]*tui.RoundProgress, len(labels))
	for i, label := range labels {
		var match []*tui.RoundProgress
		for _, pr := range prs {
			key := inspPRLabel(repoName[pr.RepoID], pr.Number)
			if g := progress[pr.ID]; g != nil && (label == key || strings.HasPrefix(label, key+" ")) {
				match = append(match, g)
			}
		}
		if len(match) == 1 {
			out[i] = match[0]
		}
	}
	return out
}

// statusDashData maps a report onto the dashboard. Cells are formatted as
// statusRender prints them; PR references are the short form the act
// commands resolve (name#N for the default owner, owner/name#N otherwise).
func statusDashData(r statusReport, defaultRepo string) tui.StatusData {
	now := r.GeneratedAt
	ago := func(t *time.Time) time.Duration { // 0 means never
		if t == nil || t.IsZero() {
			return 0
		}
		return max(now.Sub(*t), time.Nanosecond)
	}
	out := tui.StatusData{GeneratedAt: now, Warnings: slices.Clone(r.Warnings)}

	dm := r.Daemon
	out.Daemon = tui.DaemonInfo{Running: dm.Running, PID: dm.PID, Launchd: dm.Launchd, Skew: statusSafe(dm.Skew, 0)}
	if dm.Running && dm.StartedAt != nil {
		out.Daemon.Uptime = tui.HumanDuration(now.Sub(*dm.StartedAt))
	}
	out.Activity = tui.ActivityInfo{LastPoll: ago(dm.LastPoll), LastTick: ago(dm.LastTick), LastReconcile: ago(dm.LastReconcile),
		PollsFailing: screenPollsFailing(dm.PollsFailing)}
	if g := r.GitHub; g.Remaining != nil {
		out.GitHub.Remaining = *g.Remaining
		if g.Limit != nil {
			out.GitHub.Limit = *g.Limit
		}
		if g.ResetAt != nil && g.ResetAt.After(now) {
			out.GitHub.ResetIn = g.ResetAt.Sub(now)
		}
	}
	out.Rounds = tui.RoundsInfo{Active: r.Rounds.Active, Max: r.Rounds.Max, ActivePRs: slices.Clone(r.Rounds.PRs)}
	if r.Notes != nil {
		out.Notes = statusSafe(r.Notes.Line, 0)
	}
	switch ag := r.Agents; {
	case ag == nil:
		out.Agents.Error = "not checked"
	case !ag.HerdrUp:
		out.Agents.Error = "herdr unreachable"
	default:
		out.Agents = tui.AgentsInfo{CodexWorking: ag.WorkingCodex, CodexMax: ag.MaxCodex, ClaudeWorking: ag.WorkingClaude}
		for _, k := range ag.WorkingOther {
			out.Agents.Other = append(out.Agents.Other, tui.KindCount{Kind: k.Kind, Working: k.Working})
		}
	}
	out.Disk.MinGB = r.Disk.MinGB
	switch {
	case r.Disk.Error != "":
		out.Warnings = append(out.Warnings, "disk: "+r.Disk.Error)
	case r.Disk.FreeBytes > 0:
		out.Disk.FreeGB = float64(r.Disk.FreeBytes) / (1 << 30)
	}
	for _, p := range r.Pauses {
		out.Pauses = append(out.Pauses, tui.Pause{Key: statusSafe(p.Scope, 0), Reason: statusPauseText(p, now), Fix: statusSafe(p.Fix, 0)})
	}

	for _, v := range r.Slots {
		row := tui.SlotRow{Name: v.Slot.Name, Folder: inspTilde(v.Slot.Path), SlotState: statusSlotState(v),
			DBs: statusSlotDBs(v, r.Databases), Disk: statusSlotDisk(v)}
		if v.PR != nil {
			row.PRRef = actRefLabel(defaultRepo, v.Slot.RepoFullName, v.PR.Number)
			row.PRState, row.URL, row.PRGHState = screenPRState(*v.PR), v.PR.URL, v.PR.GHState
			row.PRMergedUnreviewed, row.PRFlagDismissed = v.PR.MergedUnreviewed(), v.PR.FlagDismissed()
		}
		out.Slots = append(out.Slots, row)
	}
	queue := func(q statusPRLine) tui.PRRow {
		row := tui.PRRow{Ref: actRefLabel(defaultRepo, q.Repo, q.Number), Title: actClean(q.Title), State: q.State,
			Next: statusSafe(q.Next, 0), URL: q.url}
		if q.author != "" {
			row.Author = "@" + actClean(q.author)
		}
		if a := cmp.Or(q.ActivityAt, q.UpdatedAt); a != nil {
			row.Age = actAgo(now, *a)
		}
		if q.rec != nil {
			row.Review, row.GHState = reviewFactsOf(*q.rec), q.rec.GHState
			row.MergedUnreviewed, row.FlagDismissed = q.rec.MergedUnreviewed(), q.rec.FlagDismissed()
		}
		return row
	}
	for _, q := range r.Queue {
		out.Queue = append(out.Queue, queue(q))
	}
	for _, q := range r.Closing {
		out.Queue = append(out.Queue, queue(q))
	}

	for _, a := range r.Attention {
		out.Attention = append(out.Attention, tui.AttentionRow{Subject: statusSafe(a.Subject, 0), Message: statusSafe(a.Message, 0),
			Fix: statusSafe(a.Fix, 0)})
	}
	if r.Databases && r.OrphanDBs > 0 {
		out.Attention = append(out.Attention, tui.AttentionRow{Subject: "databases",
			Message: fmt.Sprintf("%d orphan databases", r.OrphanDBs), Fix: "magnum cleanup --orphans --dry-run"})
	}
	for _, x := range r.External {
		c := statusExternalCells(x)
		out.Manual = append(out.Manual, tui.ManualRow{Folder: c[0], Branch: c[1], PRRef: c[2], GitHub: c[3], DBs: c[4], Agents: c[5], Disk: c[6]})
	}
	return out
}

// screenPRState is a PR's state as the screens show it and act on it: a PR
// `magnum ignore` muted reads "ignored", as on the board (prsRowState), so the
// dashboard's U asks to stop ignoring it.
func screenPRState(pr store.PR) string {
	if pr.Muted && store.Deref(pr.SkipReason) == engine.SkipIgnored && pr.State != store.PRClosed && pr.State != store.PRReleased {
		return "ignored"
	}
	return pr.State
}

// reviewFactsOf is what the y/N question before a review says about pr:
// its head, the last reviewed head and who reviewed it when, and the
// commits since. The poller's count is used only when it was taken for
// the current head and the current reviewed head; otherwise it is left
// out and the question says the head moved.
func reviewFactsOf(pr store.PR) *tui.ReviewFacts {
	f := &tui.ReviewFacts{HeadSHA: pr.HeadSHA, ReviewedSHA: store.Deref(pr.ReviewedSHA),
		ReviewedBy: actClean(store.Deref(pr.LastReviewLogin))}
	if pr.ReviewedAt != nil {
		f.ReviewedAt = *pr.ReviewedAt
	}
	s := pr.SinceReview
	if s == nil || s.Error != "" || s.Head == "" || s.Head != pr.HeadSHA {
		return f
	}
	switch {
	case s.Source == store.SinceFromReviewed && s.Base == f.ReviewedSHA:
	case s.Source == store.SinceFromReview && f.ReviewedSHA == "":
		f.ReviewedSHA = s.Base // no verified review yet: the identity's latest GitHub review
	default:
		return f // the whole PR (nothing reviewed), or counted against an older review
	}
	f.SinceReview = &tui.ReviewDelta{Base: "reviewed", BaseSHA: s.Base, Commits: s.Commits, Files: max(s.Files, 0),
		Additions: s.Additions, Deletions: s.Deletions, Truncated: s.Files < 0}
	f.SinceReview.MergedBase, f.SinceReview.RawBase = sinceBases(*s)
	return f
}
