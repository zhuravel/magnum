package cli

// The render half of `magnum status`: the human view of a statusReport and
// of one PR's or slot's detail card. JSON output (--json) keeps the raw
// values; everything printed here that came from outside magnum (PR titles,
// errors, pause details, pane lines) goes through statusSafe first.

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// statusSafe makes untrusted text (PR titles, errors, pause details, pane
// lines, branch names) safe for one terminal line: control characters,
// including ESC, CR and newlines, become spaces (actClean). limit > 0 also
// cuts the text to that many runes.
func statusSafe(s string, limit int) string {
	s = actClean(s)
	if limit > 0 {
		s = textx.Clip(s, limit)
	}
	return s
}

// statusRender prints the human view.
func statusRender(w io.Writer, r statusReport) {
	statusRenderHeader(w, r)
	statusRenderSlots(w, r)
	statusRenderManual(w, r)
	statusRenderQueue(w, r)
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "warning: %s\n", statusSafe(warn, 0))
	}
	if r.Detail != nil {
		fmt.Fprintln(w)
		statusRenderDetail(w, *r.Detail, r.GeneratedAt)
	}
}

// statusRenderHeader prints the daemon, activity, GitHub, rounds, disk and
// pauses lines.
func statusRenderHeader(w io.Writer, r statusReport) {
	now := r.GeneratedAt
	dm := r.Daemon
	daemon := "not running"
	if dm.Running {
		daemon = fmt.Sprintf("running (pid %d", dm.PID)
		if dm.StartedAt != nil {
			daemon += ", up " + tui.HumanDuration(now.Sub(*dm.StartedAt))
		}
		daemon += ")"
	}
	daemon += ", launchd " + statusSafe(dm.Launchd, 0)
	fmt.Fprintf(w, "daemon:   %s\n", daemon)
	if dm.Skew != "" {
		fmt.Fprintf(w, "build:    %s\n", statusSafe(dm.Skew, 0))
	}
	if line := statusPromptsText(dm); line != "" {
		fmt.Fprintf(w, "prompts:  %s\n", statusSafe(line, 0))
	}
	poll := "never"
	if dm.LastPoll != nil {
		poll = inspAgo(now, dm.LastPoll)
	}
	tick := "never"
	if dm.LastTick != nil {
		tick = inspAgo(now, dm.LastTick)
	}
	for _, f := range screenPollsFailing(dm.PollsFailing) {
		poll += ", " + statusSafe(f.Text(now), 0)
	}
	fmt.Fprintf(w, "activity: last poll %s, last tick %s", poll, tick)
	if dm.LastReconcile != nil {
		fmt.Fprintf(w, ", last reconcile %s", inspAgo(now, dm.LastReconcile))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "github:   %s\n", statusGitHubText(r.GitHub, now))
	if r.Codex != nil {
		fmt.Fprintf(w, "codex:    %s\n", statusCodexText(*r.Codex, now))
	}
	if r.Retro != nil {
		fmt.Fprintf(w, "retro:    %s\n", statusRetroText(*r.Retro, now))
	}
	if r.Notes != nil {
		fmt.Fprintf(w, "notes:    %s\n", statusSafe(r.Notes.Line, 0))
	}
	if a := r.AutoApproved; a != nil {
		fmt.Fprintf(w, "approvals: auto-approved: %d today, %d standing\n", a.Today, a.Standing)
	}
	for _, m := range r.Machine {
		fmt.Fprintf(w, "machine:  %s\n", statusMachineText(m, now))
	}
	fmt.Fprintf(w, "rounds:   %s\n", statusRoundsText(r))
	fmt.Fprintf(w, "disk:     %s\n", statusDiskText(r.Disk))
	if len(r.Pauses) == 0 {
		fmt.Fprintf(w, "pauses:   none\n")
		return
	}
	fmt.Fprintf(w, "pauses:\n")
	for _, p := range r.Pauses {
		fmt.Fprintln(w, "  "+statusSafe(p.Scope, 0)+": "+statusPauseText(p, now))
		if p.Fix != "" {
			fmt.Fprintf(w, "    fix: %s\n", statusSafe(p.Fix, 0))
		}
	}
}

// statusMachineText is a machine line's value: "6 rounds of talkable:
// `bundle exec rspec`: no test database for the worktree, last 09:24" (the
// repository without its owner; the command and the error are the judge's
// text, cleaned and cut).
func statusMachineText(m statusMachine, now time.Time) string {
	name := m.Repo
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	s := textx.Count(m.Rounds, "round", "rounds") + " of " + statusSafe(name, 40)
	if cmd := strings.TrimSpace(statusSafe(m.Cmd, 60)); cmd != "" {
		s += ": `" + cmd + "`"
	}
	if msg := strings.TrimSpace(statusSafe(m.Error, 160)); msg != "" {
		s += ": " + msg
	}
	return s + ", last " + inspClock(now, m.Last)
}

// statusPromptsText is the prompts line's value while a running daemon's
// prompt snapshot is behind the files on disk: when it was taken, how many
// files changed since and which; "" otherwise (the line is left out).
func statusPromptsText(dm statusDaemon) string {
	if !dm.Running || dm.PromptsChanged <= 0 || dm.PromptsLoadedAt == nil {
		return ""
	}
	s := engine.PromptsLine(*dm.PromptsLoadedAt, dm.PromptsChanged)
	if s == "" {
		return ""
	}
	if len(dm.PromptsChangedFiles) > 0 {
		s += " (" + strings.Join(dm.PromptsChangedFiles, ", ") + ")"
	}
	return s + "; they take effect at the next restart"
}

// statusGitHubText is the GitHub line's value: the rate budget left.
func statusGitHubText(g statusGitHub, now time.Time) string {
	if g.Remaining == nil {
		return "rate budget unknown (no poll yet)"
	}
	budget := strconv.Itoa(*g.Remaining)
	if g.Limit != nil {
		budget += "/" + strconv.Itoa(*g.Limit)
	}
	if g.ResetAt != nil {
		budget += ", resets " + inspAgo(now, g.ResetAt)
	}
	return budget + " points left"
}

// statusCodexText is the codex line's value: the budget used, its window
// and reset, and what the [usage] caps do at that level.
func statusCodexText(u statusCodexUsage, now time.Time) string {
	s := fmt.Sprintf("%d%% used", u.Percent)
	switch u.WindowMinutes {
	case 0:
	case 7 * 24 * 60:
		s += " of the weekly limit"
	default:
		s += " of the " + tui.HumanDuration(time.Duration(u.WindowMinutes)*time.Minute) + " limit"
	}
	if u.Plan != "" {
		s += " (" + statusSafe(u.Plan, 20) + ")"
	}
	if u.ResetsAt != nil {
		s += ", resets " + inspClock(now, *u.ResetsAt) + " (" + inspAgo(now, u.ResetsAt) + ")"
	}
	pct := float64(u.Percent)
	switch {
	case u.Hard > 0 && pct >= u.Hard:
		s += fmt.Sprintf("; at the hard cap %g%%: rounds that need Codex pause", u.Hard)
	case u.Soft > 0 && pct >= u.Soft:
		s += fmt.Sprintf("; at the soft cap %g%%: full re-reviews wait", u.Soft)
	case u.Soft > 0 || u.Hard > 0:
		s += fmt.Sprintf("; caps %g%%/%g%%", u.Soft, u.Hard)
	}
	if u.Pace > 0 && (u.Hard <= 0 || pct < u.Hard) {
		s += fmt.Sprintf("; pace %.1fx", u.Pace)
		var reach []string
		if u.SoftAt != nil {
			reach = append(reach, fmt.Sprintf("%g%% %s", u.Soft, inspClock(now, *u.SoftAt)))
		}
		if u.HardAt != nil {
			reach = append(reach, fmt.Sprintf("%g%% %s", u.Hard, inspClock(now, *u.HardAt)))
		}
		switch {
		case len(reach) > 0:
			s += ": " + strings.Join(reach, ", ")
		case u.Soft > 0 || u.Hard > 0:
			s += ": within the window"
		}
	}
	return s
}

// statusRetroText is the retro line's value: when the last retro finished
// and how it went, then the real misses waiting for a lesson and the PRs
// waiting for the settle delay.
func statusRetroText(ro statusRetro, now time.Time) string {
	s := "never"
	if l := ro.Last; l != nil {
		finished := l.Finished
		if finished.IsZero() {
			finished = l.At
		}
		s = inspAgo(now, &finished) + ", "
		switch {
		case l.Stopped != "":
			s += "stopped: " + statusSafe(l.Stopped, 0)
		default:
			s += textx.Count(l.PRs, "PR", "PRs") + ", " + strconv.Itoa(l.Classified) + " classified"
			if l.Failed > 0 {
				s += ", " + strconv.Itoa(l.Failed) + " failed"
			}
		}
	}
	if ro.NewMisses != nil {
		s += " · " + textx.Count(*ro.NewMisses, "new miss", "new misses")
	}
	if ro.Settling != nil && *ro.Settling > 0 {
		s += " · " + textx.Count(*ro.Settling, "PR waits", "PRs wait") + " for the " + ro.Settle + " settle delay"
	}
	return s
}

// statusRoundsText is the rounds line's value: rounds in flight and, when
// herdr was asked, the working agents.
func statusRoundsText(r statusReport) string {
	rounds := fmt.Sprintf("%d/%d active", r.Rounds.Active, r.Rounds.Max)
	if len(r.Rounds.PRs) > 0 {
		rounds += " (" + strings.Join(r.Rounds.PRs, ", ") + ")"
	}
	if ag := r.Agents; ag != nil {
		if ag.HerdrUp {
			rounds += fmt.Sprintf("; working agents: codex %d/%d, claude %d", ag.WorkingCodex, ag.MaxCodex, ag.WorkingClaude)
			for _, k := range ag.WorkingOther {
				rounds += fmt.Sprintf(", %s %d", statusSafe(k.Kind, 0), k.Working)
			}
		} else {
			rounds += "; herdr unreachable"
		}
	}
	return rounds
}

// statusDiskText is the disk line's value: free space and the floor.
func statusDiskText(d statusDisk) string {
	switch {
	case d.Error != "":
		return "unknown: " + statusSafe(d.Error, 0)
	case d.FreeBytes > 0:
		disk := fmt.Sprintf("%s free in %s (min %d GB)", tui.HumanBytes(int64(d.FreeBytes)), statusSafe(inspTilde(d.Path), 0), d.MinGB)
		if d.MinGB > 0 && d.FreeBytes < uint64(d.MinGB)<<30 {
			disk += "  LOW: provisioning refused"
		}
		return disk
	}
	return "unknown"
}

// statusRenderSlots prints the SLOTS table.
func statusRenderSlots(w io.Writer, r statusReport) {
	fmt.Fprintf(w, "\nSLOTS\n")
	if len(r.Slots) == 0 {
		fmt.Fprintf(w, "  none (`magnum slots provision` creates the pool)\n")
	} else {
		tw := inspTable(w)
		fmt.Fprintln(tw, "SLOT\tFOLDER\tPR\tSTATE\tDATABASES\tDISK")
		for _, v := range r.Slots {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", statusSafe(v.Slot.Name, 0), statusSafe(inspTilde(v.Slot.Path), 0), statusSlotPR(v),
				statusSlotState(v), statusSlotDBs(v, r.Databases), statusSlotDisk(v))
		}
		tw.Flush()
	}
	if r.Databases && r.OrphanDBs > 0 {
		fmt.Fprintf(w, "  %d orphan databases (see `magnum cleanup --orphans --dry-run`)\n", r.OrphanDBs)
	}
}

// statusRenderManual prints the manual worktrees table (--all).
func statusRenderManual(w io.Writer, r statusReport) {
	if len(r.External) == 0 {
		return
	}
	fmt.Fprintf(w, "\nMANUAL WORKTREES (read-only)\n")
	tw := inspTable(w)
	fmt.Fprintln(tw, "FOLDER\tBRANCH\tPR\tGITHUB\tDATABASES\tAGENTS\tDISK")
	for _, x := range r.External {
		fmt.Fprintln(tw, strings.Join(statusExternalCells(x), "\t"))
	}
	tw.Flush()
	fmt.Fprintf(w, "  PR numbers with ? are guessed from the branch name and may be a Jira key\n")
}

// statusRenderQueue prints the QUEUE, CLOSED-pending and ATTENTION lists.
func statusRenderQueue(w io.Writer, r statusReport) {
	fmt.Fprintf(w, "\nQUEUE (%d)\n", len(r.Queue))
	for _, q := range r.Queue {
		fmt.Fprintf(w, "  %-18s %-17s %s  %s\n", inspPRLabel(q.Repo, q.Number), statusSafe(q.State, 0), statusSafe(q.Next, 0)+statusQueueMarkText(q, r.GeneratedAt),
			statusSafe(q.Title, 50))
	}
	if len(r.Closing) > 0 {
		fmt.Fprintf(w, "\nCLOSED, pending release (%d)\n", len(r.Closing))
		for _, q := range r.Closing {
			fmt.Fprintf(w, "  %-18s %-17s %s\n", inspPRLabel(q.Repo, q.Number), statusSafe(q.State, 0), statusSafe(q.Next, 0))
		}
	}
	if len(r.Attention) > 0 {
		fmt.Fprintf(w, "\nATTENTION (%d)\n", len(r.Attention))
		for _, a := range r.Attention {
			fmt.Fprintf(w, "  %s: %s\n", statusSafe(a.Subject, 0), statusSafe(a.Message, 140))
			if a.Fix != "" {
				fmt.Fprintf(w, "    fix: %s\n", statusSafe(a.Fix, 0))
			}
		}
	}
}

// statusQueueMarkText is what a queue line adds after its wait, as the
// board's row says it: " · snoozed → 18:00" (unless the wait is the snooze)
// and " · 3 open".
func statusQueueMarkText(q statusPRLine, now time.Time) string {
	var s string
	if q.SnoozedUntil != nil && !strings.Contains(q.Next, "snoozed") {
		s += " · snoozed → " + inspClock(now, *q.SnoozedUntil)
	}
	if q.Open > 0 {
		s += fmt.Sprintf(" · %d open", q.Open)
	}
	return s
}

// statusPauseText is a pause's reason with its start, length and end, the
// review requests it holds, and its detail: "lunch since 21:04 (19h ago),
// for 20h until 17:04 (in 1h) · 6 requests held".
func statusPauseText(p statusPause, now time.Time) string {
	s := statusSafe(p.Reason, 0)
	if p.Since != nil {
		s += " since " + inspClock(now, *p.Since) + " (" + inspAgo(now, p.Since) + ")"
		if p.Until != nil && p.Until.After(*p.Since) {
			s += ", for " + tui.HumanDuration(p.Until.Sub(*p.Since))
		}
	}
	if p.Until != nil {
		if p.Until.After(now) {
			s += " until " + inspClock(now, *p.Until) + " (" + inspAgo(now, p.Until) + ")"
		} else {
			s += " (ends on the next tick)"
		}
	}
	if p.Held > 0 {
		s += " · " + textx.Count(p.Held, "request", "requests") + " held"
	}
	if p.Using != "" {
		s += ", using " + statusSafe(p.Using, 0)
	}
	if p.Detail != "" {
		s += " — " + statusSafe(p.Detail, 100)
	}
	return s
}

// statusExternalCells are a manual worktree's FOLDER, BRANCH, PR, GITHUB,
// DATABASES, AGENTS and DISK cells.
func statusExternalCells(x inventory.ExternalView) []string {
	branch := x.Branch
	if x.Detached {
		branch = "(detached " + textx.ShortSHA(x.Head) + ")"
	}
	pr := "-"
	if x.PRNumber > 0 {
		pr = "#" + strconv.Itoa(x.PRNumber)
		if !x.PRConfirmed {
			pr += "?"
		}
	}
	gh := statusSafe(x.GHState, 0)
	if gh == "" {
		gh = "-"
	}
	dbs := "-"
	if n := statusCountPresent(x.Databases); n > 0 {
		dbs = fmt.Sprintf("%d %s", n, inspMB(statusSumMB(x.Databases)))
	}
	agentsCell := "-"
	if len(x.Agents) > 0 {
		var names []string
		for _, a := range x.Agents {
			names = append(names, a.Agent+":"+string(a.Status))
		}
		agentsCell = statusSafe(strings.Join(names, ","), 0)
	}
	disk := "-"
	if x.SizeKB != nil {
		disk = tui.HumanBytes(*x.SizeKB * 1024)
	}
	return []string{statusSafe(inspTilde(x.Path), 0), statusSafe(branch, 40), pr, gh, dbs, agentsCell, disk}
}

func statusSlotPR(v inventory.SlotView) string {
	if v.PR == nil {
		return "-"
	}
	return fmt.Sprintf("#%d %s", v.PR.Number, v.PR.State)
}

func statusSlotState(v inventory.SlotView) string {
	s := v.Slot.State
	var flags []string
	if v.Slot.Pinned {
		flags = append(flags, "pinned")
	}
	if v.Slot.HoldReason != nil {
		flags = append(flags, "hold:"+*v.Slot.HoldReason)
	}
	if v.Slot.DirtySchema {
		flags = append(flags, "dirty_schema")
	}
	if !v.Exists {
		flags = append(flags, "missing")
	}
	flags = append(flags, v.Drift...)
	if len(flags) > 0 {
		s += " [" + statusSafe(strings.Join(inspDedupe(flags), ","), 0) + "]"
	}
	return s
}

func statusSlotDBs(v inventory.SlotView, listed bool) string {
	if len(v.Databases) == 0 {
		return "-"
	}
	if !listed {
		return fmt.Sprintf("%d expected (mysql?)", len(v.Databases))
	}
	expected := 0
	for _, db := range v.Databases {
		if db.Expected {
			expected++
		}
	}
	return fmt.Sprintf("%d/%d %s", statusCountPresent(v.Databases), expected, inspMB(statusSumMB(v.Databases)))
}

func statusSlotDisk(v inventory.SlotView) string {
	if v.SizeKB == nil {
		return "-"
	}
	return tui.HumanBytes(*v.SizeKB * 1024)
}

func statusCountPresent(dbs []inventory.DBView) int {
	n := 0
	for _, d := range dbs {
		if d.Present {
			n++
		}
	}
	return n
}

func statusSumMB(dbs []inventory.DBView) float64 {
	var s float64
	for _, d := range dbs {
		if d.Present {
			s += d.SizeMB
		}
	}
	return s
}

func inspDedupe(ss []string) []string {
	var out []string
	for _, s := range ss {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// statusRenderDetail prints one PR's (or slot's) card.
func statusRenderDetail(w io.Writer, d statusDetail, now time.Time) {
	if d.PR == nil {
		if d.Slot != nil {
			fmt.Fprintf(w, "slot %s (%s, %s)\n  folder: %s\n  no PR assigned\n", statusSafe(d.Slot.Slot.Name, 0), d.Slot.Slot.Kind, d.Slot.Slot.State,
				statusSafe(inspTilde(d.Slot.Slot.Path), 0))
			statusRenderDBs(w, d.Slot)
		}
		return
	}
	pr := d.PR
	fmt.Fprintf(w, "%s#%d  %s\n", statusSafe(d.Repo, 0), pr.Number, statusSafe(store.Deref(pr.Title), 0))
	fmt.Fprintf(w, "  url:       %s\n", statusSafe(pr.URL, 0))
	state := fmt.Sprintf("%s (GitHub %s", pr.State, statusSafe(pr.GHState, 0))
	if pr.IsDraft {
		state += ", draft"
	}
	state += ")"
	if pr.Pinned {
		state += ", pinned"
	}
	if pr.Muted {
		state += ", muted"
	}
	if z := d.Snooze; z != nil {
		state += ", snoozed until " + inspClock(now, z.Until)
		if z.By != "" {
			state += " by " + statusSafe(z.By, 0)
		}
	}
	fmt.Fprintf(w, "  state:     %s\n", state)
	fmt.Fprintf(w, "  next:      %s\n", statusSafe(d.Next, 0))
	if a := d.Attention; a != nil {
		fmt.Fprintf(w, "  why:       %s\n", statusSafe(a.Cause, 0))
		if a.Step != "" {
			fmt.Fprintf(w, "  step:      %s\n", statusSafe(a.Step, 0))
		}
		fmt.Fprintf(w, "  fix:       %s\n", statusSafe(a.Fix, 0))
		for i, l := range a.Tail {
			label := "             "
			if i == 0 {
				label = "  output:    "
			}
			fmt.Fprintf(w, "%s%s\n", label, statusSafe(l, 160))
		}
	} else if e := store.Deref(pr.LastError); e != "" {
		fmt.Fprintf(w, "  error:     %s\n", statusSafe(errorSummary(e), 0))
	}
	author := store.Deref(pr.AuthorLogin)
	if author == "" {
		author = "ghost"
	}
	fmt.Fprintf(w, "  author:    %s; posts as %s\n", statusSafe(author, 0), statusSafe(inspOrDash(pr.Identity), 0))
	head := textx.ShortSHA(pr.HeadSHA)
	if rs := store.Deref(pr.ReviewedSHA); rs != "" {
		head += ", reviewed " + textx.ShortSHA(rs)
		if pr.ReviewedAt != nil {
			head += " " + inspAgo(now, pr.ReviewedAt)
		}
		if ev := store.Deref(pr.LastReviewEvent); ev != "" {
			head += " (" + ev + ")"
		}
	}
	fmt.Fprintf(w, "  head:      %s\n", statusSafe(head, 0))
	if f := d.Findings; f != nil {
		fmt.Fprintf(w, "  findings:  %s\n", statusSafe(findingsSentence(*f), 0))
	}
	switch {
	case d.Slot != nil:
		sha := store.Deref(d.Slot.Slot.CheckedOutSHA)
		fmt.Fprintf(w, "  folder:    %s @ %s (slot %s, %s)\n", statusSafe(inspTilde(d.Slot.Slot.Path), 0), inspOrDash(textx.ShortSHA(sha)),
			statusSafe(d.Slot.Slot.Name, 0), d.Slot.Slot.State)
		statusRenderDBs(w, d.Slot)
	case d.LastFolder != "":
		fmt.Fprintf(w, "  folder:    none (last in %s)\n", statusSafe(inspTilde(d.LastFolder), 0))
	default:
		fmt.Fprintf(w, "  folder:    none\n")
	}
	notes := "no"
	if d.Notes {
		notes = "yes"
	}
	fmt.Fprintf(w, "  notes:     %s\n", notes)
	statusRenderSessions(w, d.Sessions)
	statusRenderHistory(w, d.Runs, now, d.isJudge)
	statusRenderTimings(w, d.LastRound)
}

// statusRenderSessions prints the agent sessions table and, under it, each
// session's copy-paste resume command.
func statusRenderSessions(w io.Writer, sessions []statusSession) {
	if len(sessions) == 0 {
		return
	}
	fmt.Fprintf(w, "  sessions:\n")
	tw := inspTable(w)
	for _, s := range sessions {
		status := store.Deref(s.AgentStatus)
		if s.Live != "" {
			status = s.Live
		}
		pane := "-"
		if s.HerdrPaneID != nil {
			pane = "pane " + *s.HerdrPaneID
		}
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\n", statusSafe(actRoleName(s.Role), 0), s.State, statusSafe(inspOrDash(status), 0),
			statusSafe(inspOrDash(store.Deref(s.AgentName)), 0), statusSafe(pane, 0))
	}
	tw.Flush()
	for _, s := range sessions {
		if s.Resume != "" {
			fmt.Fprintf(w, "    %s: %s\n", statusSafe(actRoleName(s.Role), 0), statusSafe(s.Resume, 0))
		}
	}
}

func statusRenderDBs(w io.Writer, v *inventory.SlotView) {
	if v == nil || len(v.Databases) == 0 {
		return
	}
	var parts []string
	for _, db := range v.Databases {
		p := statusSafe(db.Name, 0)
		switch {
		case db.Present:
			p += " " + inspMB(db.SizeMB)
		case db.Expected:
			p += " (missing)"
		}
		parts = append(parts, p)
	}
	fmt.Fprintf(w, "  databases: %s\n", strings.Join(parts, "\n             "))
}

// statusRenderHistory prints one line per round: the judge's verdict plus the
// reviewers' report statuses.
func statusRenderHistory(w io.Writer, runs []store.Run, now time.Time, isJudge func(string) bool) {
	if isJudge == nil {
		isJudge = func(role string) bool { return actIsJudge(nil, role) }
	}
	if len(runs) == 0 {
		fmt.Fprintf(w, "  reviews:   none yet\n")
		return
	}
	type round struct {
		n      int
		judge  []store.Run
		others []store.Run
	}
	var rounds []*round
	byN := map[int]*round{}
	for _, r := range runs {
		rd := byN[r.Round]
		if rd == nil {
			rd = &round{n: r.Round}
			byN[r.Round] = rd
			rounds = append(rounds, rd)
		}
		if isJudge(r.Role) {
			rd.judge = append(rd.judge, r)
		} else {
			rd.others = append(rd.others, r)
		}
	}
	slices.SortFunc(rounds, func(a, b *round) int { return b.n - a.n })
	fmt.Fprintf(w, "  reviews:\n")
	for _, rd := range rounds {
		for _, j := range rd.judge {
			line := fmt.Sprintf("    round %d %-9s %s %-9s", rd.n, j.Kind, textx.ShortSHA(j.TargetSHA), j.State)
			if ev := store.Deref(j.ReviewEvent); ev != "" {
				line += " " + statusSafe(ev, 0)
			} else if oc := store.Deref(j.Outcome); oc != "" {
				line += " " + statusSafe(oc, 0)
			}
			line += "  " + inspAgo(now, &j.CreatedAt)
			if u := store.Deref(j.ReviewURL); u != "" {
				line += "  " + statusSafe(u, 0)
			}
			if e := store.Deref(j.Error); e != "" && j.State == store.RunFailed {
				line += "  (" + statusSafe(e, 80) + ")"
			}
			fmt.Fprintln(w, line)
		}
		if len(rd.others) > 0 {
			var parts []string
			for _, o := range rd.others {
				st := store.Deref(o.Outcome)
				if st == "" {
					st = o.State
				}
				parts = append(parts, statusSafe(actRoleName(o.Role)+" "+st, 0))
			}
			prefix := fmt.Sprintf("    round %d", rd.n)
			if len(rd.judge) > 0 {
				prefix = "     " + strings.Repeat(" ", len(strconv.Itoa(rd.n))+5)
			}
			fmt.Fprintf(w, "%s reviewers: %s\n", prefix, strings.Join(parts, ", "))
		}
	}
}

func inspOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// findingsSentence is a review summary on one line: the decision (and what
// was posted instead), the findings by priority, the simplifications and the
// earlier findings.
func findingsSentence(f store.ReviewSummary) string {
	decision := map[string]string{store.VerdictBlocking: "request changes", store.VerdictNonBlocking: "comment",
		store.VerdictClean: "approve"}[f.Verdict]
	want := map[string]string{store.VerdictBlocking: "REQUEST_CHANGES", store.VerdictNonBlocking: "COMMENT",
		store.VerdictClean: "APPROVE"}[f.Verdict]
	s := cmp.Or(decision, f.Verdict)
	if f.Event != "" && want != "" && !strings.EqualFold(f.Event, want) {
		s += " (posted as " + strings.ToLower(strings.ReplaceAll(f.Event, "_", " ")) + ")"
	}
	s += fmt.Sprintf(" at %s: P0 %d · P1 %d · P2 %d · P3 %d", textx.ShortSHA(f.SHA), f.Counts[0], f.Counts[1], f.Counts[2], f.Counts[3])
	if f.Simplifications > 0 {
		s += fmt.Sprintf(" · %d simplifications", f.Simplifications)
	}
	if f.Fixed+f.Open+f.Answered > 0 {
		s += fmt.Sprintf("; earlier: %d fixed, %d open, %d answered", f.Fixed, f.Open, f.Answered)
	}
	return s
}
