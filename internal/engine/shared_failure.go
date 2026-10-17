package engine

// Launch failures shared across PRs (DECISIONS "The daemon records each
// agent CLI's version and names a launch failure shared across PRs"): on
// 2026-10-16 four PRs' codex-judge launches died with the same pane line
// within the hour, and each PR's attention line named only its own
// failure. The daemon says so once and pauses nothing: the operator
// decides.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
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

// launchLine is one PR's agent ending with a pane's error line.
type launchLine struct {
	line, pr, label, role string // pr: owner/name#N (lower case), label: name#N
}

// noteSharedFailures records EventSharedFailure, with one urgent toast, for
// each pane error line that two or more PRs' agents ended with within
// sharedFailureWindow and that no such event named within
// sharedFailureQuiet. The lines are the CLI's own, as agents read them
// from the pane (paneError): a launch that never came up whose pane magnum
// closed (pr.launch_cleared's last_line), a prompt herdr refused because
// the agent was gone (the run's error, agents.PaneErrorOf), and a lost
// agent's pane magnum closed (pr.panes_closed's last_lines). Nothing is
// paused.
func (e *Engine) noteSharedFailures(ctx context.Context) {
	now := e.now()
	lines := e.launchLines(ctx, now.Add(-sharedFailureWindow))
	var said map[string]bool // the lines named within sharedFailureQuiet, read once some line qualifies
	for _, line := range orderedLines(lines) {
		var group []launchLine
		for _, l := range lines {
			if l.line == line && !slices.ContainsFunc(group, func(g launchLine) bool { return g.pr == l.pr }) {
				group = append(group, l)
			}
		}
		if len(group) < 2 {
			continue
		}
		if said == nil {
			said = e.sharedFailuresSaid(ctx, now.Add(-sharedFailureQuiet))
		}
		if said[line] {
			continue
		}
		said[line] = true
		e.sayShared(ctx, line, group)
	}
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

// launchLines are the PRs' agents that ended with a pane error line since.
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
			out = append(out, launchLine{line: data.LastLine, pr: pr, label: label, role: data.Role})
		}
		for _, role := range slices.Sorted(maps.Keys(data.LastLines)) {
			if line := data.LastLines[role]; line != "" {
				out = append(out, launchLine{line: line, pr: pr, label: label, role: role})
			}
		}
	}
	runs, err := e.st.RunsOfRoundsSince(ctx, since)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "read the runs that failed")
	}
	for _, r := range runs {
		line := agents.PaneErrorOf(deref(r.Error))
		at := cmp.Or(deref(r.EndedAt), deref(r.SubmittedAt), r.CreatedAt)
		if line == "" || at.Before(since) {
			continue
		}
		name := r.Repo[strings.LastIndexByte(r.Repo, '/')+1:]
		out = append(out, launchLine{line: line, pr: strings.ToLower(fmt.Sprintf("%s#%d", r.Repo, r.Number)),
			label: fmt.Sprintf("%s#%d", name, r.Number), role: r.Role})
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
// way", the line, the PRs and a recent change of the roles' CLI.
func (e *Engine) sayShared(ctx context.Context, line string, group []launchLine) {
	slices.SortFunc(group, func(a, b launchLine) int { // by repository, then by number
		ar, an, _ := strings.Cut(a.pr, "#")
		br, bn, _ := strings.Cut(b.pr, "#")
		return cmp.Or(strings.Compare(ar, br), cmp.Compare(len(an), len(bn)), strings.Compare(an, bn))
	})
	var roles, prs, labels, kinds []string
	for _, g := range group {
		prs, labels = append(prs, g.pr), append(labels, g.label)
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
	e.event(ctx, "warn", subject, EventSharedFailure,
		fmt.Sprintf("%s within %s: %s (%s)%s; nothing is paused", what, humanDuration(sharedFailureWindow), line, strings.Join(labels, ", "), changed),
		map[string]any{"last_line": line, "prs": prs, "roles": roles})
	sum := sha256.Sum256([]byte(line))
	e.urgent("shared-failure:"+hex.EncodeToString(sum[:8]), "magnum: "+what,
		fmt.Sprintf("%s (%s)%s. Nothing is paused: look at the CLI.", textx.Clip(line, sharedToastRunes), strings.Join(labels, ", "), changed),
		sharedFailureQuiet)
}

// andList joins words with commas and a last "and".
func andList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}
