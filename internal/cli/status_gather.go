package cli

// The gather half of `magnum status`: statusGather reads the registry, kv,
// inventory and herdr into a statusReport, one focused function per section.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
	"github.com/zhuravel/magnum/internal/usage"
)

var (
	statusQueued  = []string{store.PRQueued, store.PRRereviewPending}
	statusClosing = []string{store.PRClosed, store.PRReleasing}
	statusAttn    = []string{store.PRNeedsAttention, store.PRPaused}
)

// statusGather reads the registry, kv, inventory and herdr into one report.
// Only a store failure is an error; other sources become warnings.
func statusGather(ctx context.Context, d statusDeps, o statusOptions) (statusReport, error) {
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	r := statusReport{GeneratedAt: now, Pauses: []statusPause{}, Queue: []statusPRLine{}, Closing: []statusPRLine{},
		Attention: []statusAttention{}, Drift: []inventory.Finding{}, Slots: []inventory.SlotView{}, Rounds: statusRounds{PRs: []string{}}}
	kv := statusKV{ctx: ctx, st: d.Store}

	statusGatherDaemon(ctx, d, kv, &r)
	statusGatherGitHub(kv, now, &r) // before the pauses: the budget pause comes first
	statusGatherUsage(d, kv, &r)
	statusGatherRetro(ctx, d, kv, &r)
	statusGatherNotes(ctx, d, &r)
	statusGatherPauses(d, kv, &r)
	statusGatherDisk(d, &r)
	if err := statusGatherPRs(ctx, d, now, &r); err != nil {
		return r, err
	}
	statusGatherAgents(ctx, d, &r)
	if err := statusGatherInventory(ctx, d, o, &r); err != nil {
		return r, err
	}
	if o.Ref != "" {
		det, err := statusGatherDetail(ctx, d, r, o.Ref, now)
		if err != nil {
			return r, err
		}
		r.Detail = det
	}
	return r, nil
}

// statusKV reads kv rows for status; a missing, unreadable or malformed row
// is simply absent.
type statusKV struct {
	ctx context.Context
	st  *store.Store
}

func (kv statusKV) get(key string) (string, bool) {
	v, ok, err := kv.st.GetKV(kv.ctx, key)
	if err != nil || !ok {
		return "", false
	}
	return v, true
}

func (kv statusKV) getTime(key string) *time.Time {
	v, ok := kv.get(key)
	if !ok || v == "" {
		return nil
	}
	t, err := store.ParseTime(v)
	if err != nil {
		return nil
	}
	return &t
}

func (kv statusKV) getInt(key string) *int {
	v, ok := kv.get(key)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

// statusGatherDaemon fills the daemon block: pidfile, launchd and the
// daemon's own heartbeat rows.
func statusGatherDaemon(ctx context.Context, d statusDeps, kv statusKV, r *statusReport) {
	if d.DaemonPID != nil {
		if pid, err := d.DaemonPID(); err == nil {
			r.Daemon.PID, r.Daemon.Running = pid, pid > 0
		} else {
			r.Warnings = append(r.Warnings, "daemon pidfile: "+err.Error())
		}
	}
	r.Daemon.Launchd = "unknown"
	if d.Launchd != nil {
		if info, err := d.Launchd(ctx); err == nil {
			r.Daemon.Launchd, r.Daemon.LaunchdPID = string(info.State), info.PID
		} else {
			r.Warnings = append(r.Warnings, "launchctl: "+err.Error())
		}
	}
	r.Daemon.StartedAt = kv.getTime(store.KVDaemonStartedAt)
	r.Daemon.LastTick = kv.getTime(store.KVDaemonLastTick)
	r.Daemon.LastPoll = kv.getTime(store.KVDaemonLastPoll)
	r.Daemon.LastReconcile = kv.getTime(store.KVDaemonLastReconcile)
	if v, ok := kv.get(store.KVHerdrUp); ok {
		up := v == "1"
		r.Daemon.HerdrUp = &up
	}
	if v, ok := kv.get(engine.KVDaemonDraining); ok {
		if dr, ok := engine.ParseDrain(v); ok {
			since := dr.Since
			r.Daemon.DrainingSince, r.Daemon.DrainerPID = &since, dr.PID
		}
	}
	if b, ok := engine.ReadDaemonBuild(ctx, d.Store); ok {
		r.Daemon.Build = &b
		if r.Daemon.Running {
			modTime := d.ModTime
			if modTime == nil {
				modTime = engine.FileModTime
			}
			r.Daemon.Skew = engine.SkewNote(b, d.Version, modTime)
		}
	}
	r.Daemon.PromptsLoadedAt = kv.getTime(engine.KVPromptsLoadedAt)
	if n := kv.getInt(engine.KVPromptsChanged); n != nil && *n > 0 {
		r.Daemon.PromptsChanged = *n
		if v, _ := kv.get(engine.KVPromptsChangedFiles); v != "" {
			r.Daemon.PromptsChangedFiles = strings.Split(v, ",")
		}
	}
}

// statusGatherUsage fills the Codex budget the daemon last read (nil when
// it never found a Codex rate-limit snapshot).
func statusGatherUsage(d statusDeps, kv statusKV, r *statusReport) {
	pct := kv.getInt(engine.KVUsageCodexPercent)
	if pct == nil {
		return
	}
	u := &statusCodexUsage{Percent: *pct, ResetsAt: kv.getTime(engine.KVUsageCodexResetsAt), ReportedAt: kv.getTime(engine.KVUsageCodexAt)}
	if w := kv.getInt(engine.KVUsageCodexWindow); w != nil {
		u.WindowMinutes = *w
	}
	u.Plan, _ = kv.get(engine.KVUsageCodexPlan)
	if d.Config != nil {
		u.Soft, u.Hard = d.Config.Usage.CodexSoft, d.Config.Usage.CodexHard
	}
	if u.ResetsAt != nil {
		if p, ok := usage.PaceOf(float64(u.Percent), u.WindowMinutes, *u.ResetsAt, r.GeneratedAt); ok && p.Used > 0 {
			u.Pace = math.Round(p.Ratio()*100) / 100
			reach := func(pct float64) *time.Time {
				if at, ok := p.Reach(pct); ok {
					return &at
				}
				return nil
			}
			u.SoftAt, u.HardAt = reach(u.Soft), reach(u.Hard)
		}
	}
	r.Codex = u
}

// statusGatherRetro fills the retro: the last one's summary and the new
// misses. A retro nobody asked for (no [learn] enabled, none ever ran), an
// unreadable summary or a failed count is absence, not an error.
func statusGatherRetro(ctx context.Context, d statusDeps, kv statusKV, r *statusReport) {
	ro := &statusRetro{Enabled: d.Config != nil && d.Config.Learn.Enabled}
	if v, ok := kv.get(engine.KVRetroLast); ok {
		var last engine.RetroSummary
		if json.Unmarshal([]byte(v), &last) == nil {
			ro.Last = &last
		}
	}
	if !ro.Enabled && ro.Last == nil {
		return
	}
	if n, err := d.Store.CountMisses(ctx, store.MissFilter{Classes: []string{store.MissMiss}, States: []string{store.MissNew}}); err == nil {
		ro.NewMisses = &n
	}
	if d.Config != nil && d.Config.Learn.Settle.Duration > 0 {
		lc, now := d.Config.Learn, r.GeneratedAt
		q := store.RetroQuery{Since: now.Add(-lc.Lookback.Duration), Until: now.Add(-lc.Settle.Duration)}
		if n, err := d.Store.RetroSettling(ctx, q); err == nil && n > 0 {
			ro.Settling, ro.Settle = &n, tui.HumanDuration(lc.Settle.Duration)
		}
	}
	r.Retro = ro
}

// statusGatherNotes fills the repository notes in sum (notesOverview): left
// out when no repository has notes; a registry that cannot be read is a
// warning.
func statusGatherNotes(ctx context.Context, d statusDeps, r *statusReport) {
	n := config.DefaultNotes()
	if d.Config != nil {
		n = d.Config.Notes
	}
	limits := notes.Limits{MaxBytes: n.MaxBytes, MaxLine: n.MaxLine, MaxHarnessFiles: n.MaxHarnessFiles, MaxHarnessBytes: n.MaxHarnessBytes}
	rows, err := notesOverview(ctx, d.Layout, d.Store, limits, r.GeneratedAt)
	if err != nil {
		r.Warnings = append(r.Warnings, "notes: "+err.Error())
	}
	if len(rows) > 0 {
		r.Notes = &statusNotes{Line: notesSummaryLine(rows), Repos: rows}
	}
}

// statusGatherGitHub fills the GitHub rate budget and, while polling waits
// for the reset, the "rate budget low" pause.
func statusGatherGitHub(kv statusKV, now time.Time, r *statusReport) {
	r.GitHub = statusGitHub{Remaining: kv.getInt(store.KVGHRemaining), Limit: kv.getInt(store.KVGHLimit), ResetAt: kv.getTime(store.KVGHReset)}
	if t := kv.getTime(store.KVGHPollPausedUntil); t != nil && t.After(now) {
		r.GitHub.PollPausedUntil = t
		r.Pauses = append(r.Pauses, statusPause{Scope: "github", Reason: "rate budget low", Until: t,
			Detail: "polling waits for the GitHub rate-limit reset"})
	}
}

// statusGatherPauses lists what holds automation back: the daemon pause,
// per-tool pauses, models limited on their own (sessions use a fallback
// model meanwhile), per-watch identity-leak pauses and unhealthy
// identities.
func statusGatherPauses(d statusDeps, kv statusKV, r *statusReport) {
	if v, _ := kv.get(store.KVDaemonPaused); v == "1" {
		reason, _ := kv.get(store.KVDaemonPausedReason)
		if reason == "" {
			reason = "magnum pause"
		}
		r.Pauses = append(r.Pauses, statusPause{Scope: "daemon", Reason: reason, Until: kv.getTime(store.KVDaemonPausedUntil),
			Since: kv.getTime(engine.KVDaemonPausedAt), Held: store.Deref(kv.getInt(engine.KVDaemonPausedHeld)), Fix: "magnum resume"})
	}
	if since := r.Daemon.DrainingSince; since != nil {
		r.Pauses = append(r.Pauses, statusDrainPause(d, r.Daemon, r.GeneratedAt, *since))
	}
	if until := kv.getTime(engine.KVInfraPausedUntil); until != nil {
		reason, _ := kv.get(engine.KVInfraPausedReason)
		detail, _ := kv.get(engine.KVInfraPausedDetail)
		r.Pauses = append(r.Pauses, statusPause{Scope: "infra", Reason: strings.TrimSpace("infrastructure failure " + reason),
			Detail: detail, Until: until,
			Fix: "no PR is charged; magnum probes `git ls-remote` and resumes by itself (fix SSH: `ssh-add -l`, or the network), or `magnum resume` to retry now"})
	}
	for _, tool := range rolesAllKinds(d.Config) {
		until := kv.getTime(store.KVToolPausedUntil(tool))
		if until == nil {
			continue
		}
		reason, _ := kv.get(store.KVToolPausedReason(tool))
		detail, _ := kv.get(store.KVToolPausedDetail(tool))
		p := statusPause{Scope: tool, Reason: reason, Detail: detail, Until: until}
		switch {
		case reason == string(agents.HealthLoginRequired):
			p.Fix = rolesLoginCommand(tool) + " (magnum re-checks every minute)"
		case reason == engine.BudgetPauseReason:
			p.Fix = "rounds that need " + tool + " resume by themselves once the Codex budget is below [usage] codex_hard (raise it, or set it to 0, to go on)"
		default:
			p.Fix = "wait, or `magnum resume --tool " + tool + "` to retry now"
		}
		r.Pauses = append(r.Pauses, p)
	}
	for _, tool := range rolesAllKinds(d.Config) {
		for _, l := range agents.ModelLimits(kv.ctx, kv.st, d.Config, tool, r.GeneratedAt) {
			until := l.Until
			r.Pauses = append(r.Pauses, statusPause{Scope: tool, Reason: l.Model + " limited", Until: &until, Using: l.Using})
		}
	}
	if d.Config == nil {
		return
	}
	seen := map[string]bool{}
	for _, w := range d.Config.Watches {
		owner := strings.ToLower(w.Owner)
		if seen[owner] {
			continue
		}
		seen[owner] = true
		if v, ok := kv.get(store.KVWatchPaused(owner)); ok && v != "" {
			r.Pauses = append(r.Pauses, statusPause{Scope: "watch:" + w.Owner, Reason: "identity leak", Detail: v,
				Fix: "check the leaked review on GitHub, then `magnum resume --watch " + w.Owner + "`"})
		}
	}
	for _, id := range d.Config.Identities {
		check, _ := kv.get(store.KVIdentityCheck(id.Name))
		tickErr, _ := kv.get(store.KVIdentityTickError(id.Name))
		if check != "fail" && tickErr == "" {
			continue
		}
		detail, _ := kv.get(store.KVIdentityError(id.Name))
		if detail == "" {
			detail = tickErr
		}
		r.Pauses = append(r.Pauses, statusPause{Scope: "identity:" + id.Name, Reason: "identity unhealthy", Detail: detail,
			Fix: "magnum identities check --name " + id.Name})
	}
}

// statusGatherDisk fills the free space of the pool volume and the lowest
// free-space floor any pool (or the daemon) demands.
func statusGatherDisk(d statusDeps, r *statusReport) {
	r.Disk.Path = d.DiskPath
	if d.Config != nil {
		r.Disk.MinGB = d.Config.Daemon.MinFreeDiskGB
		for _, p := range d.Config.Pools {
			r.Disk.MinGB = max(r.Disk.MinGB, p.MinFreeDiskGB)
		}
	}
	if d.DiskFree != nil && d.DiskPath != "" {
		if free, err := d.DiskFree(d.DiskPath); err == nil {
			r.Disk.FreeBytes = free
		} else {
			r.Disk.Error = err.Error()
		}
	}
}

// statusGatherPRs buckets the registry's PRs into rounds in flight, the
// queue, the closed-pending-release list and the attention list.
func statusGatherPRs(ctx context.Context, d statusDeps, now time.Time, r *statusReport) error {
	st := d.Store
	repos, err := st.ListRepos(ctx)
	if err != nil {
		return err
	}
	repoByID := map[int64]store.Repo{}
	for _, rp := range repos {
		repoByID[rp.ID] = rp
	}
	prs, err := st.ListPRs(ctx, store.PRFilter{})
	if err != nil {
		return err
	}
	if d.Config != nil {
		r.Rounds.Max = d.Config.Daemon.MaxConcurrentReviews
	}
	for _, pr := range prs {
		repo := repoByID[pr.RepoID].FullName()
		line := statusPRLine{Repo: repo, Number: pr.Number, Title: store.Deref(pr.Title), State: pr.State, Next: statusNextShort(ctx, st, pr, now),
			Forced: pr.Forced, ReviewRequested: pr.ReviewRequested, UpdatedAt: pr.GHUpdatedAt, ActivityAt: pr.Activity(),
			url: pr.URL, author: store.Deref(pr.AuthorLogin), rec: &pr}
		switch {
		case slices.Contains(prInFlight, pr.State):
			r.Rounds.Active++
			label := inspPRLabel(repo, pr.Number)
			if _, ok := statusDeltaCheck(ctx, st, pr); ok {
				label += " (delta check)"
			}
			r.Rounds.PRs = append(r.Rounds.PRs, label)
		case slices.Contains(statusQueued, pr.State):
			r.Queue = append(r.Queue, line)
		case slices.Contains(statusClosing, pr.State):
			r.Closing = append(r.Closing, line)
		case slices.Contains(statusAttn, pr.State):
			ref := inspPRLabel(repo, pr.Number)
			msg := pr.State
			fix := "fix the cause, then `magnum review " + ref + "`"
			if why, ok := prAttention(ctx, st, pr, ref); ok {
				msg, fix = why.Summary, why.Fix
			} else if e := store.Deref(pr.LastError); e != "" {
				msg += ": " + errorSummary(e)
			}
			if pr.State == store.PRPaused {
				fix = "resumes with a continue prompt when the tool pause ends"
			}
			r.Attention = append(r.Attention, statusAttention{Subject: inspPRLabel(repo, pr.Number), Message: msg, Fix: fix})
		}
	}
	statusSortQueue(r.Queue)
	if d.Config != nil {
		r.Attention = append(r.Attention, statusMergedUnreviewed(prs, repoByID, d.Config.Board.RecentClosed.Duration, now)...)
	}
	return nil
}

// statusMergedUnreviewed lists the PRs GitHub merged before magnum reviewed
// their last push (PR.MergedUnreviewed) as attention rows, newest merge first:
// "merged unreviewed at 15:04" (the date added when not today). Only a merge
// (merged_at, else closed_at) within the board's recent_closed window of now
// counts; 0 turns the list off, and a PR with neither time is skipped. There
// is no fix to name: the merge is done, the row says what went unreviewed.
func statusMergedUnreviewed(prs []store.PR, repoByID map[int64]store.Repo, window time.Duration, now time.Time) []statusAttention {
	type merged struct {
		at  time.Time
		row statusAttention
	}
	var found []merged
	for _, pr := range prs {
		if !pr.MergedUnreviewed() {
			continue
		}
		at := store.Deref(pr.MergedAt)
		if at.IsZero() {
			at = store.Deref(pr.ClosedAt)
		}
		if window <= 0 || at.IsZero() || now.Sub(at) > window {
			continue
		}
		found = append(found, merged{at, statusAttention{Subject: inspPRLabel(repoByID[pr.RepoID].FullName(), pr.Number),
			Message: "merged unreviewed at " + inspClock(now, at)}})
	}
	slices.SortStableFunc(found, func(a, b merged) int { return b.at.Compare(a.at) })
	rows := make([]statusAttention, len(found))
	for i, m := range found {
		rows[i] = m.row
	}
	return rows
}

// statusGatherAgents counts the working agent panes herdr reports; a herdr
// that does not answer is recorded as an error, not a failure.
func statusGatherAgents(ctx context.Context, d statusDeps, r *statusReport) {
	if d.Herdr == nil {
		return
	}
	ag := &statusAgents{}
	if d.Config != nil {
		ag.MaxCodex = d.Config.Daemon.MaxTotalWorkingCodex
	}
	snap, err := d.Herdr.Snapshot(ctx)
	if err != nil {
		ag.Error = err.Error()
	} else {
		ag.HerdrUp = true
		ag.WorkingCodex = agents.CountWorking(snap.Agents, agents.KindCodex)
		ag.WorkingClaude = agents.CountWorking(snap.Agents, agents.KindClaude)
		if d.Config != nil {
			for _, k := range rolesKindNames(d.Config) {
				if k != agents.KindCodex && k != agents.KindClaude {
					ag.WorkingOther = append(ag.WorkingOther, statusKindCount{Kind: k, Working: agents.CountWorking(snap.Agents, k)})
				}
			}
		}
	}
	r.Agents = ag
}

// statusGatherInventory scans the slots, manual worktrees and databases and
// turns broken, held and unsafe-drift findings into attention rows. A scan
// failure fails status.
func statusGatherInventory(ctx context.Context, d statusDeps, o statusOptions, r *statusReport) error {
	if d.Inventory == nil {
		return nil
	}
	inv, err := d.Inventory.Scan(ctx, inventory.Options{Sizes: o.Sizes, External: o.All, GitHubStates: o.All && !o.NoGitHub})
	if err != nil {
		return err
	}
	r.Slots = inv.Slots
	r.External = inv.External
	r.Databases = inv.DatabasesListed
	r.OrphanDBs = len(inv.OrphanDBs)
	r.Drift = inv.Drift
	r.Warnings = append(r.Warnings, inv.Warnings...)
	for _, sv := range inv.Slots {
		switch {
		case sv.Slot.HoldReason != nil:
			r.Attention = append(r.Attention, statusAttention{Subject: "slot " + sv.Slot.Name,
				Message: "held: " + *sv.Slot.HoldReason, Fix: "check the folder, then `magnum slots unpin " + sv.Slot.Name + "`"})
		case sv.Slot.State == store.SlotBroken:
			msg := "broken"
			if e := store.Deref(sv.Slot.LastError); e != "" {
				msg += ": " + e
			}
			r.Attention = append(r.Attention, statusAttention{Subject: "slot " + sv.Slot.Name, Message: msg,
				Fix: "magnum slots repair " + sv.Slot.Name})
		}
	}
	for _, f := range inv.Drift {
		if f.Safe {
			continue
		}
		r.Attention = append(r.Attention, statusAttention{Subject: f.Subject, Message: f.Kind + ": " + f.Message})
	}
	return nil
}

// statusSortQueue orders the queue like the dispatcher: forced first, then
// review-requested, then oldest GitHub activity.
func statusSortQueue(q []statusPRLine) {
	flag := func(b bool) int {
		if b {
			return 0
		}
		return 1
	}
	updated := func(l statusPRLine) time.Time {
		if l.UpdatedAt == nil {
			return time.Time{}
		}
		return *l.UpdatedAt
	}
	slices.SortStableFunc(q, func(a, b statusPRLine) int {
		if c := flag(a.Forced) - flag(b.Forced); c != 0 {
			return c
		}
		if c := flag(a.ReviewRequested) - flag(b.ReviewRequested); c != 0 {
			return c
		}
		return updated(a).Compare(updated(b))
	})
}

// statusWait is the daemon's account of why a PR waiting for a round
// (queued, rereview_pending) has none yet (engine.KVPRWait); ok is false
// for other states or when no daemon recorded one.
func statusWait(ctx context.Context, st *store.Store, pr store.PR) (engine.Wait, bool) {
	if st == nil || (pr.State != store.PRQueued && pr.State != store.PRRereviewPending) {
		return engine.Wait{}, false
	}
	v, ok, err := st.GetKV(ctx, engine.KVPRWait(pr.ID))
	if err != nil || !ok {
		return engine.Wait{}, false
	}
	return engine.ParseWait(v)
}

// statusNextShort is the compact "what next" of a queue row: the daemon's
// wait reason ("re-review · quiet → 14:09") when it recorded one, else
// statusNextWithGate.
func statusNextShort(ctx context.Context, st *store.Store, pr store.PR, now time.Time) string {
	if w, ok := statusWait(ctx, st, pr); ok {
		return w.Short(now)
	}
	if why, ok := prAttention(ctx, st, pr, ""); ok {
		return "needs you: " + why.Summary
	}
	return statusNextWithGate(ctx, st, pr, now)
}

// statusDeltaCheck is the delta check a PR's round in flight runs
// (engine.KVPRDeltaCheck); ok is false for a PR not in flight or a round
// that is none.
func statusDeltaCheck(ctx context.Context, st *store.Store, pr store.PR) (engine.DeltaCheckRound, bool) {
	if st == nil || !slices.Contains(prInFlight, pr.State) {
		return engine.DeltaCheckRound{}, false
	}
	v, ok, err := st.GetKV(ctx, engine.KVPRDeltaCheck(pr.ID))
	if err != nil || !ok {
		return engine.DeltaCheckRound{}, false
	}
	return engine.ParseDeltaCheckRound(v)
}

// statusNextWithGate is statusNext with the daemon's last dispatch gate
// reason (store.KVPRGate) when the PR is waiting for its turn, and the
// delta check a round in flight runs.
func statusNextWithGate(ctx context.Context, st *store.Store, pr store.PR, now time.Time) string {
	if dc, ok := statusDeltaCheck(ctx, st, pr); ok {
		return tui.DeltaCheckPhrase(dc.Lines) + " in progress"
	}
	next := statusNext(pr, now)
	if st == nil || !strings.HasSuffix(next, "waiting for a slot") {
		return next
	}
	if v, ok, err := st.GetKV(ctx, store.KVPRGate(pr.ID)); err == nil && ok && v != "" {
		return strings.TrimSuffix(next, "waiting for a slot") + "waiting: " + v
	}
	return next
}

// statusNext explains what happens next to a PR.
func statusNext(pr store.PR, now time.Time) string {
	future := func(t *time.Time) bool { return t != nil && t.After(now) }
	switch pr.State {
	case store.PRBaseline:
		return "not reviewed: open before magnum began watching; a push, a review request or `magnum review` starts a review"
	case store.PRIneligible:
		return "skipped: " + store.Deref(pr.SkipReason) + " (`magnum review` forces a round)"
	case store.PRQueued, store.PRRereviewPending:
		var parts []string
		if pr.GHState == store.GHMerged {
			parts = append(parts, "post-merge review")
		}
		if pr.Forced {
			parts = append(parts, "forced")
		}
		switch {
		case future(pr.NextAttemptAt):
			parts = append(parts, fmt.Sprintf("retry %s (attempt %d)", inspAgo(now, pr.NextAttemptAt), pr.Attempts+1))
		case !pr.Forced && future(pr.NextEligibleAt):
			parts = append(parts, "eligible "+inspAgo(now, pr.NextEligibleAt))
		default:
			parts = append(parts, "waiting for a slot")
		}
		return strings.Join(parts, ", ")
	case store.PRClaiming, store.PRReviewing, store.PRVerifying:
		if pr.GHState == store.GHMerged {
			return "post-merge round in progress"
		}
		return "round in progress"
	case store.PRReviewed:
		return "watching for new pushes"
	case store.PRPaused:
		return "waiting for the tool pause to end"
	case store.PRNeedsAttention:
		return "needs you: " + errorSummary(store.Deref(pr.LastError))
	case store.PRClosed:
		if future(pr.ReleaseAfter) {
			return "release " + inspAgo(now, pr.ReleaseAfter)
		}
		return "release on the next tick"
	case store.PRReleasing:
		return "releasing its slot"
	case store.PRReleased:
		return "released"
	}
	return pr.State
}

// statusGatherDetail builds the detail card for ref (a slot name or PR reference).
func statusGatherDetail(ctx context.Context, d statusDeps, r statusReport, ref string, now time.Time) (*statusDetail, error) {
	defRepo := ""
	if d.Config != nil {
		defRepo = d.Config.Daemon.DefaultRepo
	}
	t, err := inspResolveIn(ctx, d.Store, app.RefParser{DefaultRepo: defRepo}, ref)
	if err != nil {
		return nil, err
	}
	det := &statusDetail{Sessions: []statusSession{}, Runs: []store.Run{},
		isJudge: func(role string) bool { return actIsJudge(d.Config, role) }}
	findView := func(match func(inventory.SlotView) bool) *inventory.SlotView {
		for i := range r.Slots {
			if match(r.Slots[i]) {
				v := r.Slots[i]
				return &v
			}
		}
		return nil
	}
	if t.Slot != nil {
		id := t.Slot.ID
		det.Slot = findView(func(v inventory.SlotView) bool { return v.Slot.ID == id })
		if det.Slot == nil {
			det.Slot = &inventory.SlotView{Slot: *t.Slot}
		}
	}
	if t.PR == nil {
		return det, nil
	}
	pr := *t.PR
	det.PR = &pr
	if t.Repo != nil {
		det.Repo = t.Repo.FullName()
		if why, ok := prAttention(ctx, d.Store, pr, inspPRLabel(det.Repo, pr.Number)); ok {
			det.Attention = &why
		}
		if sums, err := d.Store.LastReviewSummaries(ctx, []int64{pr.ID}); err == nil {
			if s, ok := sums[pr.ID]; ok {
				det.Findings = &s
			}
		}
		det.Notes = notesExist(d.Layout, t.Repo.Owner, t.Repo.Name)
	}
	det.Next = statusNextWithGate(ctx, d.Store, pr, now)
	if w, ok := statusWait(ctx, d.Store, pr); ok && t.Repo != nil {
		det.Next = w.Sentence(actRefLabel(defRepo, t.Repo.FullName(), pr.Number), now)
	}
	if det.Slot == nil {
		det.Slot = findView(func(v inventory.SlotView) bool {
			return (v.Slot.PRID != nil && *v.Slot.PRID == pr.ID) || (v.PR != nil && v.PR.ID == pr.ID)
		})
	}
	if det.Slot == nil {
		if as, err := d.Store.AssignmentsByPR(ctx, pr.ID); err == nil && len(as) > 0 {
			last := as[len(as)-1]
			det.LastFolder = last.Path
		}
	}
	sessions, err := d.Store.SessionsByPR(ctx, pr.ID)
	if err != nil {
		return nil, err
	}
	var snap *herdr.Snapshot
	if d.Herdr != nil && len(sessions) > 0 {
		if s, err := d.Herdr.Snapshot(ctx); err == nil {
			snap = &s
		}
	}
	for _, s := range sessions {
		if s.State == store.SessionClosed && store.Deref(s.SessionID) == "" {
			continue
		}
		ss := statusSession{Session: s, Resume: statusResume(d.Config, s)}
		if snap != nil && s.HerdrPaneID != nil {
			for _, p := range snap.Panes {
				if p.ID == *s.HerdrPaneID {
					ss.Live = string(p.AgentStatus)
				}
			}
		}
		det.Sessions = append(det.Sessions, ss)
	}
	runs, err := d.Store.RunsByPR(ctx, pr.ID)
	if err != nil {
		return nil, err
	}
	if runs != nil {
		det.Runs = runs
	}
	if det.LastRound, err = roundTimingsFor(ctx, d.Store, d.Config, pr.ID, det.Runs, now); err != nil {
		return nil, err
	}
	return det, nil
}

// statusResume is the command that resumes a session's conversation by
// hand: its agent kind's CLI with the kind's resume args, every word quoted
// for the shell (actShellQuote) so it can be pasted as is. The folder keeps
// a leading ~ unquoted so the shell still expands it (inspShellQuote).
func statusResume(cfg *config.Config, s store.Session) string {
	argv := actResumeArgv(cfg, s)
	if argv == nil {
		return ""
	}
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = actShellQuote(a)
	}
	cmd := strings.Join(words, " ")
	if cwd := store.Deref(s.Cwd); cwd != "" {
		return "cd " + inspShellQuote(inspTilde(cwd)) + " && " + cmd
	}
	return cmd
}

// inspShellQuote quotes a path for copy-paste unless it is plainly safe.
func inspShellQuote(s string) string {
	safe := true
	for _, r := range s {
		if !(r == '/' || r == '.' || r == '-' || r == '_' || r == '~' || r == '#' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	if strings.HasPrefix(s, "~/") {
		return "~/'" + strings.ReplaceAll(s[2:], "'", `'\''`) + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
