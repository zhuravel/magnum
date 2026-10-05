package tui

import (
	"slices"
	"strconv"
	"strings"
	"time"
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
	Kind      string   // initial, rereview, continue, recovery, nudge
	PostMerge bool     // a post-merge review
	Roles     []string // the roles it ran, in the order the round named them
	Requested []string // the roles asked for this round
	Reruns    []RoleRerun
	// Triaged: triage decided this round's roles; Skipped are the roles it
	// dropped and Reason its words (the model read the PR: PR content,
	// cleaned like any). EveryRole is why triage kept every role ("the diff
	// could not be read"); all empty when triage did not run.
	Triaged   bool
	Skipped   []string
	Reason    string
	EveryRole string
}

// RoleRerun is a role that ran again because Lines code lines changed
// since its last run.
type RoleRerun struct {
	Role  string
	Lines int
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
	return StageDuration(s.AgentTime) + " · " + plural(s.Rounds, "round", "rounds")
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
	if phrase != "" && w.PostMerge {
		phrase = "post-merge " + phrase
	}
	return phrase
}

// roundWhyLines say which roles the last round ran and why, one fact a line
// (the caller wraps them): the round's kind and roles, what triage decided,
// the roles a change of their code added and the ones asked for. A continue
// round runs the judge alone, which it says instead of naming it.
func (p prbPainter) roundWhyLines(w RoundWhy) []string {
	var out []string
	roles := strings.Join(w.Roles, ", ")
	if w.Kind == "continue" && len(w.Roles) <= 1 {
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
		added = append(added, r.Role+" ("+plural(r.Lines, "line", "lines")+" changed)")
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
