package cli

// The board's CI and badges from the registry: the head commit's checks
// (store.CIStatus, deduplicated per workflow and name by the poller), the
// repository's required checks (GitHub's rulesets, or [[repo]]
// required_checks), and the configured label badges ([board] badges).

import (
	"cmp"
	"slices"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// prsCI is a row's CIInfo: nil when the poller has no checks for the PR yet.
func prsCI(ci *store.CIStatus, head string, req store.RequiredChecks) *tui.CIInfo {
	if ci == nil {
		return nil
	}
	out := &tui.CIInfo{Total: ci.Total, Passed: ci.Passed, Failed: ci.Failed, Pending: ci.Pending, Skipped: ci.Skipped,
		Stale: ci.SHA != "" && head != "" && ci.SHA != head, RequiredSource: req.Source}
	out.State = ciState(ci.Failed, ci.Pending, ci.AllSkipped, len(ci.Checks))
	byWorkflow := map[string]*tui.WorkflowCI{}
	var order []string
	for _, c := range ci.Checks {
		if c.State == store.CheckFailed {
			name := c.Name
			if c.Workflow != "" {
				name = c.Workflow + " / " + c.Name
			}
			out.Failing = append(out.Failing, name)
		}
		w, ok := byWorkflow[c.Workflow]
		if !ok {
			w = &tui.WorkflowCI{Name: c.Workflow}
			byWorkflow[c.Workflow] = w
			order = append(order, c.Workflow)
		}
		w.Total++
		switch c.State {
		case store.CheckPassed:
			w.Passed++
		case store.CheckFailed:
			w.Failed++
		case store.CheckPending:
			w.Pending++
		}
	}
	slices.Sort(order)
	for _, name := range order {
		w := byWorkflow[name]
		w.State = ciState(w.Failed, w.Pending, w.Passed+w.Failed+w.Pending == 0, w.Total)
		out.Workflows = append(out.Workflows, *w)
	}
	for _, pattern := range req.Checks {
		c := requiredCheck(ci.Checks, pattern)
		if out.Stale && c.State != store.CheckFailed {
			// The checks are an older commit's: the head's are not known yet.
			c.State, c.Done, c.Total = store.CheckPending, 0, 0
		}
		out.Required = append(out.Required, c)
	}
	return out
}

// ciState folds counts into a board state: a failure wins, then anything
// running, then "every check skipped" (nothing ran), then none or passed.
func ciState(failed, pending int, allSkipped bool, checks int) string {
	switch {
	case failed > 0:
		return "failed"
	case pending > 0:
		return "pending"
	case checks == 0:
		return "none"
	case allSkipped:
		return "skipped"
	}
	return "passed"
}

// requiredCheck is one required check on the head: a name glob
// ("Completion", "rspec*") matched across workflows, the latest run of each
// matching name counting, or "workflow:<glob>" for every check of the
// matching workflows (store.RequiredRuns). State "missing" when nothing
// matched.
func requiredCheck(checks []store.CheckResult, pattern string) tui.CheckState {
	out := tui.CheckState{Name: pattern, Label: checkLabel(pattern)}
	matched := store.RequiredRuns(checks, pattern)
	out.Total = len(matched)
	if out.Total == 0 {
		out.State = "missing"
		return out
	}
	var failed, pending, skipped int
	for _, c := range matched {
		switch c.State {
		case store.CheckFailed:
			failed++
		case store.CheckPending:
			pending++
		case store.CheckSkipped:
			skipped++
		}
	}
	out.Done = out.Total - pending
	switch {
	case failed > 0:
		out.State = store.CheckFailed
	case pending > 0:
		out.State = store.CheckPending
	case skipped == len(matched):
		out.State = store.CheckSkipped
	default:
		out.State = store.CheckPassed
	}
	return out
}

// checkLabel is a required check's short name on the board: without the
// "workflow:" prefix, and a glob without its trailing wildcards and the
// separators before them ("ci / *" → "ci", "rspec*" → "rspec"); the pattern
// itself when nothing would be left ("*").
func checkLabel(pattern string) string {
	s := strings.TrimPrefix(pattern, config.RequiredWorkflowPrefix)
	if t := strings.TrimRight(s, "*?"); t != s {
		s = strings.TrimRight(t, " /:-_")
	}
	if s == "" {
		return pattern
	}
	return s
}

// prsBadges are the configured badges ([board] badges) a PR's labels carry,
// in the badges' label order. A badge key matches a label whatever their
// case and leading emoji or symbols ("Flagged" matches "🚩 Flagged").
func prsBadges(labels []string, badges map[string]config.BadgeSpec) []tui.Badge {
	if len(badges) == 0 || len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(badges))
	for k := range badges {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(badgeKey(a), badgeKey(b)) })
	var out []tui.Badge
	for _, k := range keys {
		for _, l := range labels {
			if badgeKey(l) == badgeKey(k) {
				out = append(out, tui.Badge{Label: l, Text: badges[k].Text, Color: badges[k].Color})
				break
			}
		}
	}
	return out
}

// badgeKey is a label without its case, variation selectors and leading
// emoji, symbols or spaces.
func badgeKey(s string) string {
	s = strings.ReplaceAll(s, "\ufe0f", "")
	s = strings.TrimLeftFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	return strings.ToLower(strings.TrimSpace(s))
}
