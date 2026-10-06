package tui

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/zhuravel/magnum/internal/textx"
)

// SpendInfo is what a PR's reviews cost over a window: the agent time (the
// sum of its runs' durations, to now for a run still going) and how many
// rounds those runs belong to.
type SpendInfo struct {
	Window    time.Duration // how far back it counts (7 days)
	AgentTime time.Duration
	Rounds    int
}

// RoundWhy says which roles a PR's last round ran and why: its kind (a
// continue runs the judge alone), the roles asked for it (`magnum review
// --role`, --simplify), the roles that ran again because their code changed
// (rerun_min_lines), and triage's decision.
type RoundWhy struct {
	Kind      string // initial, rereview, continue, recovery, nudge
	PostMerge bool   // a post-merge review
	// DeltaCheck: a re-review of DeltaLines changed code lines by the judge
	// alone (a delta check).
	DeltaCheck bool
	DeltaLines int
	Roles      []string // the roles it ran, in the order the round named them
	Requested  []string // the roles asked for this round
	Reruns     []RoleRerun
	// Triaged: triage decided this round's roles; Skipped are the roles it
	// dropped and Reason its words (the model read the PR: PR content,
	// cleaned like any). EveryRole is why triage kept every role ("the diff
	// could not be read"); all empty when triage did not run.
	Triaged   bool
	Skipped   []string
	Reason    string
	EveryRole string
	// At is when the round started (its engine.round_start event).
	At time.Time
}

// RoleRerun is a role that ran again because Lines code lines changed
// since its last run.
type RoleRerun struct {
	Role  string
	Lines int
}

// RoundProgress is a round in flight: when it started (the PR's
// last_round_started_at) and its roles, the ones with a run in the order
// their runs were created, then the ones the round named that have none
// yet, the judge last and its own pass right before it.
type RoundProgress struct {
	StartedAt time.Time
	Roles     []RoleProgress
}

// RoleProgress is one role of a round in flight. Started is when its run
// was prompted (zero: not started yet) and Ended when it ended (zero while
// it works); Working: its run is submitted or working; Failed: it failed or
// was abandoned. Label is the short name the state cell gives it while it
// works (the shortest of its name and aliases, see the cli's stageLabel);
// the judge is "judge" whatever its names.
type RoleProgress struct {
	Role, Label string
	Judge       bool
	// OwnPass: the judge's own pass, its runs of kind own_pass, listed as an
	// entry of its own before the judge's.
	OwnPass         bool
	Started, Ended  time.Time
	Working, Failed bool
}

// stageLabel is what the state cell calls r while it works.
func (r RoleProgress) stageLabel() string {
	switch {
	case r.Judge:
		return "judge"
	case r.Label != "":
		return r.Label
	}
	return r.Role
}

// stage is what the round is doing: the label of the one role working,
// "reviewers" while several other roles work at once, "judge" while the
// judge (its own pass or its main run) works alone and both joined by "+"
// while the judge's own pass works with them ("claude+judge",
// "reviewers+judge"); "" while none works (the readiness step before the
// reviewers, the time between stages, the verification).
func (g RoundProgress) stage() string {
	var working []RoleProgress
	judge := ""
	for _, r := range g.Roles {
		switch {
		case !r.Working:
		case r.Judge:
			judge = r.stageLabel()
		default:
			working = append(working, r)
		}
	}
	others := ""
	switch len(working) {
	case 0:
	case 1:
		others = working[0].stageLabel()
	default:
		others = "reviewers"
	}
	switch {
	case others != "" && judge != "":
		return others + "+" + judge
	case judge != "":
		return judge
	}
	return others
}

// roundElapsed is how long a round has run in whole minutes ("0m", "17m",
// "1h5m", "2d3h"): HumanDuration of the time cut to the minute, so the text
// changes at most once a minute.
func roundElapsed(d time.Duration) string {
	if d < time.Minute {
		return "0m"
	}
	return HumanDuration(d.Truncate(time.Minute))
}

// progressText is what a round in flight does and for how long at now:
// "simplify · 17m"; the time alone ("17m") when no role works or withStage
// is false.
func progressText(g RoundProgress, now time.Time, withStage bool) string {
	t := roundElapsed(max(now.Sub(g.StartedAt), 0))
	if s := g.stage(); s != "" && withStage {
		return s + " · " + t
	}
	return t
}

// cleanRoundProgress is g with its text safe to draw.
func cleanRoundProgress(g RoundProgress) RoundProgress {
	g.Roles = slices.Clone(g.Roles)
	for i := range g.Roles {
		g.Roles[i].Role, g.Roles[i].Label = cleanText(g.Roles[i].Role), cleanText(g.Roles[i].Label)
	}
	return g
}

// cleanRoundWhy is w with its text safe to draw.
func cleanRoundWhy(w RoundWhy) RoundWhy {
	w.Kind, w.Reason, w.EveryRole = cleanText(w.Kind), cleanText(w.Reason), cleanText(w.EveryRole)
	w.Roles, w.Requested, w.Skipped = cleanAll(w.Roles), cleanAll(w.Requested), cleanAll(w.Skipped)
	w.Reruns = slices.Clone(w.Reruns)
	for i := range w.Reruns {
		w.Reruns[i].Role = cleanText(w.Reruns[i].Role)
	}
	return w
}

// spendCell is a PR's agent time on the card's facts table: "9h02m · 15
// rounds"; "" without any.
func spendCell(s *SpendInfo) string {
	if s == nil {
		return ""
	}
	return StageDuration(s.AgentTime) + " · " + textx.Count(s.Rounds, "round", "rounds")
}

// spendLabel names the window of a PR's agent time: "Agent time 7d".
func spendLabel(s *SpendInfo) string {
	switch {
	case s == nil || s.Window <= 0:
		return "Agent time 7d"
	case s.Window%(24*time.Hour) == 0:
		return "Agent time " + strconv.Itoa(int(s.Window/(24*time.Hour))) + "d"
	}
	return "Agent time " + HumanDuration(s.Window)
}

// roundKindPhrase says what kind of round it was.
func roundKindPhrase(w RoundWhy) string {
	phrase := map[string]string{
		"initial":  "first review",
		"rereview": "re-review",
		"continue": "continue (finishing an interrupted round)",
		"recovery": "recovery (a fresh judge session)",
		"nudge":    "nudge",
	}[w.Kind]
	if phrase == "" {
		phrase = w.Kind
	}
	if w.DeltaCheck {
		phrase = DeltaCheckPhrase(w.DeltaLines)
	}
	if phrase != "" && w.PostMerge {
		phrase = "post-merge " + phrase
	}
	return phrase
}

// DeltaCheckPhrase names a delta check of lines changed code lines:
// "delta check (4 lines)".
func DeltaCheckPhrase(lines int) string {
	return "delta check (" + textx.Count(lines, "line", "lines") + ")"
}

// roundWhyLines say which roles the last round ran and why, one fact a line
// (the caller wraps them): the round's kind and roles, what triage decided,
// the roles a change of their code added and the ones asked for. A continue
// round and a delta check run the judge alone, which they say instead of
// naming it.
func (p prbPainter) roundWhyLines(w RoundWhy) []string {
	var out []string
	roles := strings.Join(w.Roles, ", ")
	if (w.Kind == "continue" || w.DeltaCheck) && len(w.Roles) <= 1 {
		roles = "judge only"
	}
	switch kind := roundKindPhrase(w); {
	case kind != "" && roles != "":
		out = append(out, p.st.Dim.Render(kind+":")+" "+roles)
	case kind != "":
		out = append(out, p.st.Dim.Render(kind))
	case roles != "":
		out = append(out, p.st.Dim.Render("roles:")+" "+roles)
	}

	reason := ""
	if w.Reason != "" {
		reason = `: "` + w.Reason + `"`
	}
	switch {
	case w.Triaged && len(w.Skipped) > 0:
		out = append(out, p.st.Dim.Render("triage skipped ")+strings.Join(w.Skipped, ", ")+reason)
	case w.Triaged:
		out = append(out, p.st.Dim.Render("triage kept every role")+reason)
	case w.EveryRole != "":
		out = append(out, p.st.Dim.Render("triage ran every role: ")+w.EveryRole)
	}

	// A rerun role is among the round's requested roles too, but nobody asked
	// for it: the change of its code did.
	var added, asked []string
	for _, r := range w.Reruns {
		added = append(added, r.Role+" ("+textx.Count(r.Lines, "line", "lines")+" changed)")
	}
	for _, r := range w.Requested {
		if !slices.ContainsFunc(w.Reruns, func(x RoleRerun) bool { return x.Role == r }) && !slices.Contains(asked, r) {
			asked = append(asked, r)
		}
	}
	if len(added) > 0 {
		out = append(out, p.st.Dim.Render("rerun added ")+strings.Join(added, ", "))
	}
	if len(asked) > 0 {
		out = append(out, p.st.Dim.Render("asked for: ")+strings.Join(asked, ", "))
	}
	return out
}

// progressLines are a round in flight on the card: when it started and how
// long it has run, then its roles one a line, each with when it started and
// ended and how long it took ("claude-review  started 14:02 · ended 14:15 ·
// 13m04s"), a working one the time so far ("running 17m02s"), a failed one
// in red and one without a run "not started yet"; the judge's own pass is
// "<role> own pass" ("codex-judge own pass"). Times are the card's
// StageDuration.
func (p prbPainter) progressLines(g RoundProgress) []string {
	clock := func(t time.Time) string { return t.Local().Format("15:04") }
	sep := p.st.Dim.Render(" · ")
	out := []string{p.st.Dim.Render("round started ") + clock(g.StartedAt) + sep +
		p.st.Accent.Render("running "+StageDuration(p.now.Sub(g.StartedAt)))}
	nameOf := func(r RoleProgress) string {
		if r.OwnPass {
			return r.Role + " own pass"
		}
		return r.Role
	}
	nameW := 0
	for _, r := range g.Roles {
		nameW = max(nameW, ansi.StringWidth(nameOf(r)))
	}
	for _, r := range g.Roles {
		name := nameOf(r) + spaces(nameW-ansi.StringWidth(nameOf(r))+2)
		if r.Started.IsZero() {
			out = append(out, name+p.st.Dim.Render("not started yet"))
			continue
		}
		parts := []string{p.st.Dim.Render("started ") + clock(r.Started)}
		switch {
		case r.Working:
			parts = append(parts, p.st.Accent.Render("running "+StageDuration(p.now.Sub(r.Started))))
		case !r.Ended.IsZero() && r.Failed:
			parts = append(parts, p.pal.red.Render("failed "+clock(r.Ended)), StageDuration(r.Ended.Sub(r.Started)))
		case !r.Ended.IsZero():
			parts = append(parts, p.st.Dim.Render("ended ")+clock(r.Ended), StageDuration(r.Ended.Sub(r.Started)))
		case r.Failed:
			parts = append(parts, p.pal.red.Render("failed"))
		}
		out = append(out, name+strings.Join(parts, sep))
	}
	return out
}
