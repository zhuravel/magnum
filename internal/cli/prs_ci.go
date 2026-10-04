package cli

// The board's CI and badges from the registry: the head commit's checks
// (store.CIStatus, deduplicated per workflow and name by the poller), the
// repository's required checks (GitHub's rulesets, or [[repo]]
// required_checks), and the configured label badges ([board] badges).

import (
	"cmp"
	"path"
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
		st := requiredState(ci.Checks, pattern)
		if out.Stale && st != store.CheckFailed {
			st = store.CheckPending // the checks are an older commit's: the head's are not known yet
		}
		out.Required = append(out.Required, tui.CheckState{Name: pattern, State: st})
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

// requiredState is the state of one required check on the head: a name
// glob ("Completion", "rspec*") matched across workflows, the latest run of
// each matching name counting (a check posted through the API lands in
// another workflow's suite, next to an older run), or "workflow:<glob>" for
// every check of the matching workflows. "missing" when nothing matched.
func requiredState(checks []store.CheckResult, pattern string) string {
	var matched []store.CheckResult
	if glob, ok := strings.CutPrefix(pattern, config.RequiredWorkflowPrefix); ok {
		for _, c := range checks {
			if ok, _ := path.Match(glob, c.Workflow); ok && c.Workflow != "" {
				matched = append(matched, c)
			}
		}
	} else {
		latest := map[string]store.CheckResult{}
		for _, c := range checks {
			if ok, _ := path.Match(pattern, c.Name); !ok {
				continue
			}
			if prev, seen := latest[c.Name]; !seen || newer(c, prev) {
				latest[c.Name] = c
			}
		}
		for _, c := range latest {
			matched = append(matched, c)
		}
	}
	if len(matched) == 0 {
		return "missing"
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
	switch {
	case failed > 0:
		return store.CheckFailed
	case pending > 0:
		return store.CheckPending
	case skipped == len(matched):
		return store.CheckSkipped
	}
	return store.CheckPassed
}

// newer reports whether run a is newer than b: a run not started yet (zero
// At) is the newest.
func newer(a, b store.CheckResult) bool {
	switch {
	case a.At.IsZero():
		return true
	case b.At.IsZero():
		return false
	}
	return a.At.After(b.At)
}

// prsBadges are the configured badges ([board] badges) a PR's labels carry,
// in the badges' label order. A badge key matches a label whatever their
// case and leading emoji or symbols ("Flagged" matches "🚩 Flagged").
func prsBadges(labels []string, badges map[string]string) []tui.Badge {
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
				out = append(out, tui.Badge{Label: l, Text: badges[k]})
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
