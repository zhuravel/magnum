package engine

// Launch failures shared across PRs (DECISIONS "The daemon records each
// agent CLI's version and names a launch failure shared across PRs"): on
// 2026-10-16 four PRs' codex-judge launches died with the same pane line
// within the hour, and each PR's attention line named only its own
// failure. The daemon says so once, and holds the kind when the roles share
// one (kind_hold.go).

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// EventSharedFailure is recorded (warn) when two or more PRs' agent
// launches ended with the same pane line within sharedFailureWindow, once
// per line within sharedFailureQuiet: the PRs, the roles and the line
// (data last_line, prs, roles); its subject is tool:<kind> when the roles
// share one agent kind.
const EventSharedFailure = "agents.shared_failure"

// sharedFailureWindow is how close in time the PRs' failures with one line
// come; sharedFailureQuiet is how long a line said once is not said again.
const (
	sharedFailureWindow = 30 * time.Minute
	sharedFailureQuiet  = 24 * time.Hour
)

// sharedToastRunes bounds the pane line in the toast.
const sharedToastRunes = 120

// launchLine is one PR's agent ending with a pane's error line, or one PR's
// round ending overloaded (overloadedRounds: no line, its agent kind).
type launchLine struct {
	line, pr, label, role string // pr: owner/name#N (lower case), label: name#N
	kind                  string // an overloaded round's agent kind ("" for a pane line: its role's, kindOf)
	overloaded            bool
	at                    time.Time // when the agent ended with the line (zero for an overloaded round)
}

// onePerPR is the first line of each PR among ls that keep takes, in the
// order they came: the PRs that share a failure.
func onePerPR(ls []launchLine, keep func(launchLine) bool) []launchLine {
	var out []launchLine
	for _, l := range ls {
		if keep(l) && !slices.ContainsFunc(out, func(g launchLine) bool { return g.pr == l.pr }) {
			out = append(out, l)
		}
	}
	return out
}

// noteSharedFailures records EventSharedFailure, with one urgent toast, for
// each pane error line that two or more PRs' agents ended with within
// sharedFailureWindow and that no such event named within
// sharedFailureQuiet. The lines are the CLI's own, as agents read them
// from the pane (paneError): a launch that never came up whose pane magnum
// closed (pr.launch_cleared's last_line), a prompt herdr refused because
// the agent was gone (the run's error, agents.PaneErrorOf), and a lost
// agent's pane magnum closed (pr.panes_closed's last_lines). Each such
// group gives back the attempts its line charged (refundSharedAttempts) and
// holds its roles' agent kind when they share one (holdShared), whether
// the line was named today or not.
func (e *Engine) noteSharedFailures(ctx context.Context) {
	now := e.now()
	lines := e.launchLines(ctx, now.Add(-sharedFailureWindow))
	var said map[string]bool // the lines named within sharedFailureQuiet, read once some line qualifies
	for _, line := range orderedLines(lines) {
		group := onePerPR(lines, func(l launchLine) bool { return l.line == line })
		if len(group) < 2 {
			continue
		}
		e.refundSharedAttempts(ctx, line, group)
		if said == nil {
			said = e.sharedFailuresSaid(ctx, now.Add(-sharedFailureQuiet))
		}
		held := e.holdShared(ctx, line, lines, group, said[line])
		if said[line] {
			continue
		}
		said[line] = true
		e.sayShared(ctx, line, group, held)
	}
}

// groupKinds are the agent kinds of the roles of group, in role order.
func (e *Engine) groupKinds(group []launchLine) []string {
	var roles, kinds []string
	for _, g := range group {
		if g.role != "" && !slices.Contains(roles, g.role) {
			roles = append(roles, g.role)
		}
	}
	slices.Sort(roles)
	for _, name := range roles {
		if r, ok := e.cfg.RoleByNameOrAlias(nil, name); ok && r.AgentKind() != "" && !slices.Contains(kinds, r.AgentKind()) {
			kinds = append(kinds, r.AgentKind())
		}
	}
	return kinds
}

// holdShared holds the one agent kind the roles of group (the PRs whose
// agents ended with line) share (holdKind), when two or more PRs' agents
// ended with line since the kind's last hold was lifted, so the lines that
// held it do not hold it again; it returns the held kind ("" for none: the
// roles share no single kind, an agent session's). A kind held already
// stays as it is. said: agents.shared_failure named line today, so the
// hold toasts itself.
func (e *Engine) holdShared(ctx context.Context, line string, lines, group []launchLine, said bool) string {
	kinds := e.groupKinds(group)
	if len(kinds) != 1 {
		return ""
	}
	kind := kinds[0]
	if _, ok := e.kindHoldOf(ctx, kind); ok {
		return kind
	}
	lifted, _ := e.kvTime(ctx, KVKindHoldLifted(kind))
	fresh := onePerPR(lines, func(l launchLine) bool { return l.line == line && l.at.After(lifted) })
	if len(fresh) < 2 {
		return ""
	}
	sortByPR(fresh)
	labels := make([]string, 0, len(fresh))
	for _, g := range fresh {
		labels = append(labels, g.label)
	}
	e.holdKind(ctx, kind, line, labels, said)
	if _, ok := e.kindHoldOf(ctx, kind); !ok {
		return ""
	}
	return kind
}

// orderedLines are the distinct lines of ls, in the order they came.
func orderedLines(ls []launchLine) []string {
	var out []string
	for _, l := range ls {
		if !slices.Contains(out, l.line) {
			out = append(out, l.line)
		}
	}
	return out
}

// launchLines are the PRs' agents that ended with a pane error line since:
// the launches agents cleared and the lost agents' panes it closed (their
// events), and the runs whose prompt herdr refused (never submitted).
func (e *Engine) launchLines(ctx context.Context, since time.Time) []launchLine {
	var out []launchLine
	evs, err := e.st.EventsOfKindsSince(ctx, since, agents.EventLaunchCleared, agents.EventPanesClosed)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the launches that ended")
	}
	for _, ev := range evs {
		pr, label, ok := prOfSubject(deref(ev.Subject))
		if !ok {
			continue
		}
		var data struct {
			Role      string            `json:"role"`
			LastLine  string            `json:"last_line"`
			LastLines map[string]string `json:"last_lines"`
		}
		if json.Unmarshal(ev.Data, &data) != nil {
			continue
		}
		if data.LastLine != "" {
			out = append(out, launchLine{line: data.LastLine, pr: pr, label: label, role: data.Role, at: ev.At})
		}
		for _, role := range slices.Sorted(maps.Keys(data.LastLines)) {
			if line := data.LastLines[role]; line != "" {
				out = append(out, launchLine{line: line, pr: pr, label: label, role: role, at: ev.At})
			}
		}
	}
	runs, err := e.st.RunsOfRoundsSince(ctx, since)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the runs that failed")
	}
	for _, r := range runs {
		if r.SubmittedAt != nil {
			// Its agent took the prompt: its error is not its launch's. A
			// reviewer's run the round's end abandoned carries the round's
			// error, which quotes the judge's pane line.
			continue
		}
		line := agents.PaneErrorOf(deref(r.Error))
		at := cmp.Or(deref(r.EndedAt), r.CreatedAt)
		if line == "" || at.Before(since) {
			continue
		}
		name := r.Repo[strings.LastIndexByte(r.Repo, '/')+1:]
		out = append(out, launchLine{line: line, pr: strings.ToLower(fmt.Sprintf("%s#%d", r.Repo, r.Number)),
			label: fmt.Sprintf("%s#%d", name, r.Number), role: r.Role, at: at})
	}
	return out
}

// prOfSubject reads an event's PR subject ("pr:talkable/talkable#12"): the
// PR as owner/name#N in lower case and its label (talkable#12).
func prOfSubject(subject string) (pr, label string, ok bool) {
	full, ok := strings.CutPrefix(subject, "pr:")
	repo, num, found := strings.Cut(full, "#")
	if !ok || !found || !strings.Contains(repo, "/") || num == "" {
		return "", "", false
	}
	return strings.ToLower(full), repo[strings.LastIndexByte(repo, '/')+1:] + "#" + num, true
}

// sharedFailuresSaid are the lines EventSharedFailure named since.
func (e *Engine) sharedFailuresSaid(ctx context.Context, since time.Time) map[string]bool {
	out := map[string]bool{}
	evs, err := e.st.EventsOfKindsSince(ctx, since, EventSharedFailure)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the shared failures")
	}
	for _, ev := range evs {
		var data struct {
			LastLine string `json:"last_line"`
		}
		if json.Unmarshal(ev.Data, &data) == nil && data.LastLine != "" {
			out[data.LastLine] = true
		}
	}
	return out
}

// sayShared records EventSharedFailure for line and the PRs of group (two
// or more), with one urgent toast: "2 PRs' codex-judge launches end the same
// way", the line, the PRs, a recent change of the roles' CLI and the hold
// of held, the kind holdShared held ("" for none: nothing is paused).
func (e *Engine) sayShared(ctx context.Context, line string, group []launchLine, held string) {
	sortByPR(group)
	var roles, prs, labels []string
	for _, g := range group {
		prs, labels = append(prs, g.pr), append(labels, g.label)
		if g.role != "" && !slices.Contains(roles, g.role) {
			roles = append(roles, g.role)
		}
	}
	slices.Sort(roles)
	kinds := e.groupKinds(group)
	whose := "agent"
	if len(roles) > 0 {
		whose = andList(roles)
	}
	what := fmt.Sprintf("%d PRs' %s launches end the same way", len(group), whose)
	changed, now := "", e.now() // "; the codex CLI changed at 11:24, to codex-cli 0.163.0"
	for _, kind := range kinds {
		if v, ok, err := readKVJSON[KindVersion](ctx, e, KVKindVersion(kind), "kind version"); err == nil && ok && v.ChangedWithin(now, CLIChangeRecent) {
			changed += attention.CLIChange(kind, v.ChangedAt, now) + ", to " + v.Version
		}
	}
	subject := ""
	if len(kinds) == 1 {
		subject = "tool:" + kinds[0]
	}
	paused, toast := "nothing is paused", "Nothing is paused: look at the CLI."
	if words := e.heldWordsFor(ctx, held); held != "" && words != "" {
		paused = words
		toast = heldToastWords(held) + ". Look at the CLI."
	}
	e.event(ctx, "warn", subject, EventSharedFailure,
		fmt.Sprintf("%s within %s: %s (%s)%s; %s", what, humanDuration(sharedFailureWindow), line, strings.Join(labels, ", "), changed, paused),
		map[string]any{"last_line": line, "prs": prs, "roles": roles})
	sum := sha256.Sum256([]byte(line))
	e.urgent("shared-failure:"+hex.EncodeToString(sum[:8]), "magnum: "+what,
		fmt.Sprintf("%s (%s)%s. %s", textx.Clip(line, sharedToastRunes), strings.Join(labels, ", "), changed, toast),
		sharedFailureQuiet)
}

// Launch failures shared across PRs charge no attempt (DECISIONS "A round
// that dies on a launch failure other PRs share charges no attempt"): on
// 2026-11-05 every codex-judge launch died at the TUI bootstrap with one
// pane line for two hours, and two PRs spent their three attempts four
// times over between the operator's forced reviews, for a cause neither
// had.

// eventRoundLaunchShared is recorded (info) under the PR for each round
// that charged no attempt because its error carries a pane line other PRs'
// agents ended with (sharedLaunchFailed): how many PRs share it, the retry
// and its wait (data prs, retry, wait); never the pane line, which
// agents.shared_failure names.
const eventRoundLaunchShared = "round.launch_shared"

// eventAttemptsRefunded is recorded (info) under a PR whose attempts that
// rounds ending on a pane line charged went back once other PRs' agents
// ended with that line too (refundSharedAttempts): how many and how many PRs
// share the line (data attempts, prs, to); never the line.
const eventAttemptsRefunded = "round.attempts_refunded"

// kvPRLaunchFailures holds the PR's rounds that ended on one pane line
// (launchFailures, JSON). A round of the PR that ends any other way
// deletes it (finish).
func kvPRLaunchFailures(prID int64) string { return fmt.Sprintf("pr.%d.launch_failures", prID) }

// launchFailures is the PR's record of its rounds that ended on one pane
// line: the line, the attempts those rounds charged while no other PR
// shared it (Charged, which go back once one does: refundSharedAttempts),
// and its shared failures in a row (Shared), whose next retry waits backoff
// of it.
type launchFailures struct {
	Line    string `json:"line"`
	Charged int    `json:"charged,omitempty"`
	Shared  int    `json:"shared,omitempty"`
}

// launchFailuresOf is the PR's record (the zero record: none, or
// unreadable).
func (e *Engine) launchFailuresOf(ctx context.Context, prID int64) launchFailures {
	rec, _, _ := readKVJSON[launchFailures](ctx, e, kvPRLaunchFailures(prID), "launch failures")
	return rec
}

// sharedLaunchFailed settles a failed round without charging the PR an
// attempt when its error carries a pane line (agents.PaneErrorOf) that
// another PR's agent ended with within sharedFailureWindow (launchLines,
// the lines noteSharedFailures groups): the round does not count against
// the daily cap (refund, RefundInfra), the PR goes back in line, its
// attempts untouched, and retries after backoff(n), n its shared failures
// on that line in a row (launchFailures.Shared), so the wait doubles from
// a minute up to retryMax; its last_error says the launch fails the same
// way on the PRs, and one round.launch_shared info event under the PR names
// how many. The attempts earlier rounds ending on the line charged before
// another PR shared it go back with it (launchFailures.Charged). It reports
// false, and the caller charges the attempt, for a round whose error
// carries no pane line, and for one no other PR shares, which the record
// counts as charged.
func (e *Engine) sharedLaunchFailed(ctx context.Context, job *roundJob, pr store.PR, from []string, round int, outcome, msg string) bool {
	line := agents.PaneErrorOf(msg)
	if line == "" {
		return false
	}
	subject := prSubject(job.repo, pr.Number)
	self, label, _ := prOfSubject(subject)
	group := onePerPR(e.launchLines(ctx, e.now().Add(-sharedFailureWindow)), func(l launchLine) bool { return l.pr != self && l.line == line })
	sharers := len(group) + 1
	if held := e.heldLinePRs(ctx, line); len(group) == 0 && held > 0 {
		sharers = max(held, 2) // a kind's hold names the line, its PRs further back than the window (kind_hold.go)
	}
	rec := e.launchFailuresOf(ctx, pr.ID)
	if rec.Line != line {
		rec = launchFailures{Line: line}
	}
	if sharers < 2 {
		rec.Charged, rec.Shared = rec.Charged+1, 0
		e.saveLaunchFailures(ctx, pr.ID, rec)
		return false
	}
	group = append(group, launchLine{pr: self, label: label})
	sortByPR(group)
	labels := make([]string, 0, len(group))
	for _, g := range group {
		labels = append(labels, g.label)
	}
	back := min(rec.Charged, pr.Attempts)
	pr.Attempts -= back // retryOrAttention writes them back
	rec.Charged = 0
	rec.Shared++
	e.saveLaunchFailures(ctx, pr.ID, rec)
	wait := backoff(rec.Shared)
	e.refund(ctx, job, round, RefundInfra, outcome)
	// The shared part first: a screen's one-line summary clips the end.
	e.retryOrAttention(ctx, job, pr, from, claimableState(pr), fmt.Sprintf("%s: the launch fails the same way on %d PRs (no attempt charged; retry %d in %s): %s",
		outcome, sharers, rec.Shared, humanDuration(wait), msg), false, e.now().Add(wait))
	e.event(ctx, "info", subject, eventRoundLaunchShared,
		fmt.Sprintf("the round's launch fails the same way on %d PRs (%s): no attempt charged%s; retry %d in %s (doubling up to %s)",
			sharers, strings.Join(labels, ", "), givenBack(back), rec.Shared, humanDuration(wait), humanDuration(retryMax)),
		map[string]any{"prs": sharers, "retry": rec.Shared, "wait": humanDuration(wait), "attempts_back": back})
	return true
}

// heldLinePRs is how many PRs shared line when a kind's hold that names it
// began (0: no hold names it).
func (e *Engine) heldLinePRs(ctx context.Context, line string) int {
	for _, k := range e.kinds() {
		if h, ok := e.kindHoldOf(ctx, k); ok && h.Line == line {
			return h.PRs
		}
	}
	return 0
}

// givenBack says the attempts earlier rounds on the line charged that go
// back: ", 2 attempts back"; "" for none.
func givenBack(n int) string {
	if n == 0 {
		return ""
	}
	return ", " + textx.Count(n, "attempt", "attempts") + " back"
}

// saveLaunchFailures records the PR's launch failures (kvPRLaunchFailures).
func (e *Engine) saveLaunchFailures(ctx context.Context, prID int64, rec launchFailures) {
	if e.d.DryRun {
		return
	}
	if err := writeKVJSON(ctx, e, kvPRLaunchFailures(prID), "launch failures", rec); err != nil {
		e.warnUnlessStopped(ctx, err, "record the PR's launch failures", "pr", prID)
	}
}

// refundSharedAttempts gives back, for each PR of group (the PRs whose
// agents ended with line, noteSharedFailures), the attempts its rounds
// ending on line charged while no other PR shared it (launchFailures.Charged,
// at most its attempts), once: a PR waiting in line keeps waiting with
// them back, and a PR they parked in needs attention (attention.KindFailed)
// goes back in line at once, its attention cleared. Each is one
// round.attempts_refunded info event. A PR whose round runs is left to that
// round's settle (sharedLaunchFailed gives them back too), and so is any
// other state.
func (e *Engine) refundSharedAttempts(ctx context.Context, line string, group []launchLine) {
	if e.d.DryRun {
		return
	}
	for _, g := range group {
		repo, num, _ := strings.Cut(g.pr, "#")
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		r, err := e.st.RepoByFullName(ctx, repo)
		if err != nil {
			continue
		}
		pr, err := e.st.PRByRepoNumber(ctx, r.ID, n)
		if err != nil {
			continue
		}
		rec := e.launchFailuresOf(ctx, pr.ID)
		back := min(rec.Charged, pr.Attempts)
		if rec.Line != line || back <= 0 || e.roundActive(pr.ID) {
			continue
		}
		attempts, to := pr.Attempts-back, pr.State
		why, _ := e.getKV(ctx, KVPRAttention(pr.ID))
		switch {
		case pr.State == store.PRNeedsAttention && why == attention.KindFailed:
			to = claimableState(pr)
			err = e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, to, func(u *store.PRUpdate) {
				u.Where("attempts", pr.Attempts)
				u.Set("attempts", attempts)
				u.Set("next_attempt_at", e.now())
				u.Set("last_error", fmt.Sprintf("back in line: the launch it failed on fails the same way on %d PRs (%s back)",
					len(group), textx.Count(back, "attempt", "attempts")))
			})
			if err == nil {
				e.delKV(ctx, KVPRAttention(pr.ID), kvPRAttentionRun(pr.ID))
			}
		case slices.Contains(store.ClaimableStates, pr.State) || pr.State == store.PRPaused:
			err = e.st.TransitionPR(ctx, pr.ID, []string{pr.State}, pr.State, func(u *store.PRUpdate) {
				u.Where("attempts", pr.Attempts)
				u.Set("attempts", attempts)
			})
		default:
			continue
		}
		if err != nil {
			e.log.Info("give back the attempts of a shared launch failure: the PR moved on", "pr", pr.ID, "err", err)
			continue
		}
		rec.Charged = 0
		e.saveLaunchFailures(ctx, pr.ID, rec)
		msg := fmt.Sprintf("%s back: the rounds that charged them ended on a launch failure %d PRs share (agents.shared_failure); %d of %d attempts used",
			textx.Count(back, "attempt", "attempts"), len(group), attempts, maxAttempts)
		if to != pr.State {
			msg += "; back in line from " + pr.State
		}
		e.event(ctx, "info", prSubject(r, pr.Number), eventAttemptsRefunded, msg, map[string]any{"attempts": back, "prs": len(group), "to": to})
	}
}

// sortByPR orders ls by repository, then by number.
func sortByPR(ls []launchLine) {
	slices.SortFunc(ls, func(a, b launchLine) int {
		ar, an, _ := strings.Cut(a.pr, "#")
		br, bn, _ := strings.Cut(b.pr, "#")
		return cmp.Or(strings.Compare(ar, br), cmp.Compare(len(an), len(bn)), strings.Compare(an, bn))
	})
}

// Overloads shared across PRs: on 2026-10-20 a provider outage of 26
// minutes ended 21 rounds overloaded ("stream disconnected before
// completion"), and six PRs parked within 15 minutes, each on its third
// charged attempt, 1 and then 2 minutes after the first. An overload that
// other PRs' rounds share is the provider's, not the PR's: it charges no
// attempt, and its retry waits longer each time instead.

// overloadShareWindow is how close in time other PRs' overloads come to
// share one: twice the longest wait of a shared overload's retry
// (retryMax), so PRs that retry in lockstep at that wait, their rounds
// ending in the same tick, still find each other's last overload, which a
// window of retryMax would just miss by the rounds' length. Wider than
// sharedFailureWindow, which noteSharedFailures keeps.
const overloadShareWindow = 2 * retryMax

// eventRoundResult is recorded (info) under the PR at each round's end
// (finish): its outcome, review and error, and the agent kind of the pause
// or overload the round hit (data tool).
const eventRoundResult = "engine.round_result"

// eventRoundOverloadShared is recorded (info) under the PR for each
// overloaded round that charged no attempt because other PRs shared the
// overload (sharedOverload): the kind, how many PRs share it, the retry
// and its wait (data tool, prs, retry, wait); never the pane line.
const eventRoundOverloadShared = "round.overload_shared"

// kvPROverloadShared counts the PR's overloads in a row that other PRs
// shared, as a decimal string: its next uncharged retry waits backoff of
// it. A round of the PR that ends any other way, a lone overload included,
// deletes it (finish).
func kvPROverloadShared(prID int64) string { return fmt.Sprintf("pr.%d.overload_shared", prID) }

// sharedOverload settles an overloaded round without charging the PR an
// attempt when other PRs share the overload (overloadSharers): the PR goes
// back to paused, its attempts untouched, and retries after backoff(n), n
// its shared overloads in a row (kvPROverloadShared), so the wait doubles
// from a minute up to retryMax; its last_error says the provider is
// overloaded across the PRs, and one round.overload_shared info event under
// the PR names how many share it. No kind is paused or held (only a shared
// launch line holds one, noteSharedFailures), and a PR a lone overload
// parked is never retried: only the round's own PR moves, from the round's
// states. It reports false, and the caller charges the attempt, for a lone
// overload and for one whose agent kind the round did not name.
func (e *Engine) sharedOverload(ctx context.Context, job *roundJob, pr store.PR, from []string, res pipeline.RoundResult, msg string) bool {
	if res.Pause == nil || res.Pause.Tool == "" {
		return false
	}
	subject := prSubject(job.repo, pr.Number)
	self, label, _ := prOfSubject(subject)
	tool := res.Pause.Tool
	group := e.overloadSharers(ctx, self, tool, res.Pause.Detail)
	if len(group) == 0 {
		return false
	}
	group = append(group, launchLine{pr: self, label: label})
	sortByPR(group)
	labels := make([]string, 0, len(group))
	for _, g := range group {
		labels = append(labels, g.label)
	}
	n := e.overloadsShared(ctx, pr.ID) + 1
	wait := backoff(n)
	e.setKV(ctx, kvPROverloadShared(pr.ID), strconv.Itoa(n))
	// The shared part first: a screen's one-line summary clips the end.
	e.toPaused(ctx, pr, from, fmt.Sprintf("%s: %s is overloaded across %d PRs (no attempt charged; retry %d in %s): %s",
		pipeline.OutcomeOverloaded, tool, len(group), n, humanDuration(wait), msg), e.now().Add(wait), nil)
	e.event(ctx, "info", subject, eventRoundOverloadShared,
		fmt.Sprintf("%s is overloaded across %d PRs within %s (%s): no attempt charged; retry %d in %s (doubling up to %s)",
			tool, len(group), humanDuration(overloadShareWindow), strings.Join(labels, ", "), n, humanDuration(wait), humanDuration(retryMax)),
		map[string]any{"tool": tool, "prs": len(group), "retry": n, "wait": humanDuration(wait)})
	return true
}

// overloadsShared is the PR's count of shared overloads in a row (0: none,
// or unreadable).
func (e *Engine) overloadsShared(ctx context.Context, prID int64) int {
	v, _ := e.getKV(ctx, kvPROverloadShared(prID))
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// overloadSharers are the other PRs that share an overload of the PR self
// (owner/name#N, lower case) on the agent kind tool, whose pane showed
// line: within overloadShareWindow, a round of theirs on tool ended
// overloaded (overloadedRounds), or an agent of theirs on tool ended with
// the same pane line (launchLines, the lines noteSharedFailures groups),
// one entry per PR (onePerPR, its grouping).
func (e *Engine) overloadSharers(ctx context.Context, self, tool, line string) []launchLine {
	since := e.now().Add(-overloadShareWindow)
	lines := e.overloadedRounds(ctx, since)
	if line != "" {
		lines = append(lines, e.launchLines(ctx, since)...)
	}
	return onePerPR(lines, func(l launchLine) bool {
		return l.pr != self && e.kindOf(l) == tool && (l.overloaded || l.line == line)
	})
}

// overloadedRounds are the PRs' rounds that ended overloaded since, as
// finish recorded them (eventRoundResult, with the agent kind whose pane
// showed it): no line, their kind.
func (e *Engine) overloadedRounds(ctx context.Context, since time.Time) []launchLine {
	evs, err := e.st.EventsOfKindsSince(ctx, since, eventRoundResult)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the rounds that ended")
	}
	var out []launchLine
	for _, ev := range evs {
		pr, label, ok := prOfSubject(deref(ev.Subject))
		var data struct {
			Outcome string `json:"outcome"`
			Tool    string `json:"tool"`
		}
		if !ok || json.Unmarshal(ev.Data, &data) != nil || data.Outcome != pipeline.OutcomeOverloaded || data.Tool == "" {
			continue
		}
		out = append(out, launchLine{pr: pr, label: label, kind: data.Tool, overloaded: true})
	}
	return out
}

// kindOf is the agent kind of l: an overloaded round's own, else its
// role's ("" when no declared role has the name).
func (e *Engine) kindOf(l launchLine) string {
	if l.kind != "" || l.role == "" {
		return l.kind
	}
	if r, ok := e.cfg.RoleByNameOrAlias(nil, l.role); ok {
		return r.AgentKind()
	}
	return ""
}

// andList joins words with commas and a last "and".
func andList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}
